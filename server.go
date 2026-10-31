package pglite

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// server accepts PostgreSQL connections on a Unix socket and runs them, one
// session at a time, on the backend.
//
// The backend is PostgreSQL in single-user mode: one process, one session.
// A connection that arrives while another session is active waits up to
// handoffWait for it to end (a pool replacing its connection closes the old
// one just before opening the new one) and is otherwise refused with
// SQLSTATE 53300. When a session ends the server returns the backend to
// the state of a new connection (see session.end).
type server struct {
	ln      net.Listener
	backend *backend
	log     *slog.Logger
	// settings are the set_config calls every session starts with: the
	// session defaults, then Config.Settings.
	settings []string

	slot chan struct{} // holds a token while a session runs
	done chan struct{} // closed by close

	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

const (
	// handoffWait is how long a new connection waits for the active
	// session to end before it is refused.
	handoffWait = time.Second
	// startupTimeout bounds the startup handshake.
	startupTimeout = 10 * time.Second
)

func newServer(ln net.Listener, b *backend, cfg *Config) *server {
	s := &server{
		ln:      ln,
		backend: b,
		log:     cfg.Logger,
		slot:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		conns:   map[net.Conn]struct{}{},
	}
	for _, d := range sessionDefaults {
		s.settings = append(s.settings, setConfig(d[0], d[1]))
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Settings)) {
		s.settings = append(s.settings, setConfig(name, cfg.Settings[name]))
	}
	s.wg.Go(s.acceptLoop)
	return s
}

func (s *server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
			default:
				s.log.Error("pglite: accepting connections stopped", "err", err)
			}
			return
		}
		s.mu.Lock()
		select {
		case <-s.done:
			// close has already closed the connections it knew of; this
			// one was accepted after that and would otherwise wait for
			// its startup packet until the deadline.
			s.mu.Unlock()
			conn.Close()
			return
		default:
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() {
			s.serveConn(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		})
	}
}

// close stops accepting, closes the client connections and waits for the
// sessions to finish. It does not shut the backend down.
func (s *server) close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		err := s.ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			err = errors.Join(err, c.Close())
		}
		s.mu.Unlock()
		s.wg.Wait()
		if !errors.Is(err, net.ErrClosed) {
			s.closeErr = err
		}
	})
	return s.closeErr
}

func (s *server) serveConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 64<<10)
	if err := conn.SetDeadline(time.Now().Add(startupTimeout)); err != nil {
		s.connError("setting the startup deadline", err)
		return
	}
	params, ok := s.readStartup(conn, r)
	if !ok {
		return
	}
	select {
	case s.slot <- struct{}{}:
	case <-time.After(handoffWait):
		fatal(conn, codeTooManyConnections, "too many connections: pglite serves one session at a time and another session is active")
		return
	case <-s.done:
		fatal(conn, codeAdminShutdown, "terminating connection due to administrator command")
		return
	}
	defer func() { <-s.slot }()
	if err := s.backend.err(); err != nil {
		fatal(conn, codeInternalError, err.Error())
		return
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		s.connError("clearing the startup deadline", err)
		return
	}
	sess := &session{server: s, conn: conn, r: r}
	sess.run(params)
}

// connError logs a failure on a client connection that nobody else can be
// told about, unless the server is closing and the failure is its doing.
func (s *server) connError(what string, err error) {
	select {
	case <-s.done:
	default:
		s.log.Error("pglite: connection dropped", "while", what, "err", err)
	}
}

// readStartup handles SSL and GSS encryption requests, cancel requests and
// the StartupMessage, and checks the requested database and user.
func (s *server) readStartup(conn net.Conn, r *bufio.Reader) ([][2]string, bool) {
	for {
		code, body, err := readStartup(r)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fatal(conn, codeProtocolViolation, "invalid startup packet: "+err.Error())
			}
			return nil, false
		}
		switch code {
		case sslRequest, gssEncRequest:
			if _, err := conn.Write([]byte{'N'}); err != nil {
				return nil, false
			}
			continue
		case cancelRequest:
			// Statements cannot be interrupted. PostgreSQL also closes a
			// cancel connection without a reply.
			return nil, false
		case protocolVersion3:
		default:
			fatal(conn, codeFeatureNotSupported, fmt.Sprintf("unsupported frontend protocol %d.%d: pglite supports 3.0", code>>16, code&0xffff))
			return nil, false
		}
		params, err := parseStartupParams(body)
		if err != nil {
			fatal(conn, codeProtocolViolation, "invalid startup packet: "+err.Error())
			return nil, false
		}
		user, database := "", ""
		for _, p := range params {
			switch p[0] {
			case "user":
				user = p[1]
			case "database":
				database = p[1]
			}
		}
		if database == "" {
			database = user
		}
		switch {
		case user == "":
			fatal(conn, codeInvalidAuthSpec, "no PostgreSQL user name specified in startup packet")
		case user != User:
			fatal(conn, codeInvalidAuthSpec, fmt.Sprintf("role %q does not exist: pglite serves only role %q", user, User))
		case database != Database:
			fatal(conn, codeInvalidDatabase, fmt.Sprintf("database %q does not exist: pglite serves only database %q", database, Database))
		default:
			return params, true
		}
		return nil, false
	}
}

// fatal sends a FATAL ErrorResponse. The connection is closed afterwards, so
// a write error has no one to be reported to.
func fatal(w io.Writer, code, msg string) {
	_, _ = w.Write(errorResponse("FATAL", code, msg))
}

// session is one client connection that owns the backend.
type session struct {
	server *server
	conn   net.Conn
	r      *bufio.Reader

	// unsynced is set while extended-protocol messages have been passed on
	// without a following Sync.
	unsynced bool
	// txStatus is the transaction status of the last ReadyForQuery.
	txStatus byte
}

func (c *session) run(params [][2]string) {
	b := c.server.backend
	defer c.end()
	if err := c.start(params); err != nil {
		if pgErr, ok := errors.AsType[*PgError](err); ok {
			fatal(c.conn, pgErr.Code, pgErr.Message)
		} else {
			fatal(c.conn, codeInternalError, err.Error())
		}
		return
	}
	for {
		m, err := readMessage(c.r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrUnexpectedEOF) {
				fatal(c.conn, codeProtocolViolation, err.Error())
			}
			return
		}
		var reply []byte
		switch copies := copyInCount(m); {
		case m.typ() == 'X':
			return
		case !forwarded(m.typ()):
			fatal(c.conn, codeProtocolViolation, fmt.Sprintf("invalid frontend message type %q", m.typ()))
			return
		case m.typ() == 'P' && copies > 0:
			// The data for a COPY started through the extended protocol
			// would arrive after Execute, in a later call into the backend,
			// which cannot resume a COPY (see copyFromStdinCount). pgx's
			// CopyFrom uses the simple protocol.
			reply, err = c.refuseExtended(codeFeatureNotSupported, "COPY FROM STDIN is supported through the simple query protocol only")
		case copies > 1:
			// A client sends the data for a second COPY only after the
			// first has finished, which it cannot know from one reply.
			reply = append(errorResponse("ERROR", codeFeatureNotSupported, "a query may contain one COPY FROM STDIN statement"), readyForQuery(c.txStatus)...)
		case copies == 1:
			c.unsynced = false
			reply, err = c.copyIn(m)
		default:
			// Messages the client has already sent go to the backend
			// together, up to the first one that ends with ReadyForQuery:
			// a pipeline costs one call rather than one per message, and
			// the module handles one error per call.
			batch := m
			c.track(m)
			for !syncPoint(m.typ()) {
				next, ok := c.bufferedMessage()
				if !ok {
					break
				}
				batch = append(batch, next...)
				c.track(next)
				m = next
			}
			reply, err = b.exec(batch)
		}
		if err != nil {
			fatal(c.conn, codeInternalError, err.Error())
			return
		}
		c.noteStatus(reply)
		if _, err := c.conn.Write(reply); err != nil {
			c.server.connError("writing a reply", err)
			return
		}
		if b.err() != nil {
			// The reply carried the FATAL; the backend is unusable now.
			return
		}
	}
}

// refuseExtended answers an extended-protocol message the server will not
// pass on the way the backend answers an error: an ErrorResponse, then the
// rest of the pipeline is discarded up to and including its Sync, which is
// answered with ReadyForQuery. A pipeline the backend has already started
// is closed with a Sync of its own, so the backend ends its implicit
// transaction (committing the messages before the refused one, where the
// backend's own error would have rolled them back) and reports the status;
// replies it had still buffered come before the error.
func (c *session) refuseExtended(code, msg string) ([]byte, error) {
	refusal := errorResponse("ERROR", code, msg)
	for {
		m, err := readMessage(c.r)
		if err != nil {
			return nil, err
		}
		switch m.typ() {
		case 'X':
			return nil, io.EOF
		case 'S':
			if !c.unsynced {
				return append(refusal, readyForQuery(c.txStatus)...), nil
			}
			c.unsynced = false
			b := c.server.backend
			reply, err := b.exec(syncMessage)
			if err != nil {
				return nil, err
			}
			n := len(reply) - len(readyForQuery('I'))
			if n < 0 || reply[n] != 'Z' {
				return nil, b.fail(fmt.Errorf("Sync was answered with %s, want ReadyForQuery last", describe(reply)))
			}
			return append(append(reply[:n:n], refusal...), reply[n:]...), nil
		}
	}
}

// copyInCount returns the number of COPY FROM STDIN statements in a Query
// or Parse message, and 0 for other messages.
func copyInCount(m message) int {
	switch m.typ() {
	case 'Q':
		return copyFromStdinCount(string(m.body()[:max(len(m.body())-1, 0)]))
	case 'P':
		// Parse: statement name, query, parameter types.
		if _, rest, ok := bytes.Cut(m.body(), []byte{0}); ok {
			query, _, _ := bytes.Cut(rest, []byte{0})
			return copyFromStdinCount(string(query))
		}
	}
	return 0
}

// forwarded reports whether a frontend message type is passed to the
// backend. Terminate is handled by the server; startup-phase and
// authentication messages are not valid in a session.
func forwarded(typ byte) bool {
	return strings.IndexByte("QPBDECHSFdcf", typ) >= 0
}

// syncPoint reports whether a message type makes the backend finish with
// ReadyForQuery: Query, Sync and FunctionCall.
func syncPoint(typ byte) bool {
	return typ == 'Q' || typ == 'S' || typ == 'F'
}

// track updates unsynced for a message passed to the backend.
func (c *session) track(m message) {
	switch m.typ() {
	case 'Q', 'S', 'F':
		c.unsynced = false
	case 'P', 'B', 'D', 'E', 'C', 'H':
		c.unsynced = true
	}
}

// bufferedMessage returns the next message if the reader already holds all
// of it and it can join a batch.
func (c *session) bufferedMessage() (message, bool) {
	// Peek blocks for missing bytes, so only buffered data is examined.
	if c.r.Buffered() < 5 {
		return nil, false
	}
	hdr, err := c.r.Peek(5)
	if err != nil {
		return nil, false
	}
	n := int(binary.BigEndian.Uint32(hdr[1:]))
	if n < 4 || c.r.Buffered() < 1+n || hdr[0] == 'X' || !forwarded(hdr[0]) {
		return nil, false
	}
	whole, err := c.r.Peek(1 + n)
	if err != nil || copyInCount(whole) > 0 {
		return nil, false
	}
	m, err := readMessage(c.r)
	if err != nil {
		panic(fmt.Sprintf("pglite: reading a buffered message: %v", err)) // the bytes were checked above
	}
	return m, true
}

// noteStatus records the transaction status of the last ReadyForQuery in a
// reply, which ends with it if it contains one.
func (c *session) noteStatus(reply []byte) {
	if n := len(reply); n >= 6 && reply[n-6] == 'Z' && binary.BigEndian.Uint32(reply[n-5:]) == 5 {
		c.txStatus = reply[n-1]
	}
}

// copyIn passes a query with a COPY FROM STDIN statement on together with
// the CopyData that follows it, up to CopyDone or CopyFail. The backend
// reads COPY data from its input during the COPY and treats the end of the
// input as the client going away, so the whole stream has to be there in
// one call. The client gets CopyInResponse only after it has sent its data;
// pgx sends without waiting for it. Should the COPY not run (an earlier
// statement in the query failed), the backend ignores the stray CopyData.
func (c *session) copyIn(query message) ([]byte, error) {
	b := c.server.backend
	f, err := b.inputFile()
	if err != nil {
		return nil, err
	}
	_, err = f.Write(query)
	for err == nil {
		var m message
		if m, err = readMessage(c.r); err != nil {
			break
		}
		if _, err = f.Write(m); err != nil {
			break
		}
		if t := m.typ(); t != 'd' && t != 'H' && t != 'S' {
			break // CopyDone, CopyFail, or a message COPY will reject
		}
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return nil, err
	}
	if err := b.err(); err != nil {
		return nil, err
	}
	reply, err := b.runFile()
	if err != nil {
		return nil, b.fail(err)
	}
	return reply, nil
}

// start opens the session on the backend and sends the startup reply.
//
// The module runs PostgreSQL's own startup packet processing and answers
// with an MD5 password request that it does not check; the server
// completes that exchange itself, so clients see no password request. The
// startup parameters other than user and database are then applied with
// set_config, since PostgreSQL applies them only when a backend starts,
// and the ParameterStatus messages for the ones it reports are added to
// the startup reply.
func (c *session) start(params [][2]string) error {
	b := c.server.backend
	reply, err := b.exec(startupMessage("user", User, "database", Database))
	if err != nil {
		return err
	}
	const authMD5 = 5
	if m, ok := singleMessage(reply); !ok || m.typ() != 'R' || len(m.body()) != 8 || binary.BigEndian.Uint32(m.body()) != authMD5 {
		return b.fail(fmt.Errorf("startup packet was answered with %s, want an MD5 password request", describe(reply)))
	}
	reply, err = b.exec(passwordMessage("pglite"))
	if err != nil {
		return err
	}
	var out, statuses []byte
	var keyData message
	for m, err := range messages(reply) {
		if err != nil {
			return b.fail(err)
		}
		switch m.typ() {
		case 'R':
			out = append(out, m...)
		case 'S':
			statuses = append(statuses, m...)
		case 'K':
			keyData = m
		case 'Z':
		default:
			return b.fail(fmt.Errorf("password message was answered with %s", describe(reply)))
		}
	}
	if len(out) == 0 || keyData == nil {
		return b.fail(fmt.Errorf("password message was answered with %s, want AuthenticationOk and BackendKeyData", describe(reply)))
	}

	settings, err := startupSettings(params)
	if err != nil {
		return err
	}
	// The client's parameters come after the instance's and override them.
	all := append(slices.Clone(c.server.settings), settings...)
	reply, err = b.exec(queryMessage("SELECT " + strings.Join(all, ", ")))
	if err != nil {
		return err
	}
	if _, err := parseQueryReply("startup settings", reply); err != nil {
		return err
	}
	// The module's message loop never reports changed settings (PostgreSQL
	// does that from PostgresMain, which this build does not run), so the
	// reported settings are read back and sent after the module's values.
	var sql strings.Builder
	sql.WriteString("SELECT name, current_setting(name) FROM unnest(ARRAY[")
	for i, name := range reportedParameters {
		if i > 0 {
			sql.WriteString(", ")
		}
		sql.WriteString(quoteLiteral(name))
	}
	sql.WriteString("]) AS name")
	rows, err := b.query(sql.String())
	if err != nil {
		return err
	}
	for _, row := range rows {
		if len(row) != 2 || row[0] == nil || row[1] == nil {
			return b.fail(fmt.Errorf("reported settings query returned %v", row))
		}
		statuses = append(statuses, typedMessage('S', func(b *builder) {
			b.cstring(*row[0])
			b.cstring(*row[1])
		})...)
	}
	out = append(out, statuses...)
	out = append(out, keyData...)
	out = append(out, readyForQuery('I')...)
	c.txStatus = 'I'
	_, err = c.conn.Write(out)
	return err
}

// reportedParameters are the settings PostgreSQL 17 reports to clients in
// ParameterStatus messages (GUC_REPORT).
var reportedParameters = []string{
	"application_name", "client_encoding", "DateStyle", "default_transaction_read_only",
	"in_hot_standby", "integer_datetimes", "IntervalStyle", "is_superuser",
	"scram_iterations", "server_encoding", "server_version", "session_authorization",
	"standard_conforming_strings", "TimeZone",
}

// sessionDefaults are the settings every session starts with. The module
// starts PostgreSQL with search_path=pg_catalog on its command line, which
// initdb needs, and this backend applies neither per-database settings (a
// single-user backend skips them) nor startup-packet options, so
// PostgreSQL's usual defaults are set here. RESET returns a setting to the
// command-line value. The time zone would otherwise be GMT: the module's
// environment has no zone PostgreSQL can identify.
var sessionDefaults = [][2]string{{"search_path", `"$user", public`}, {"TimeZone", "UTC"}}

// startupSettings returns the set_config calls for the startup parameters.
func startupSettings(params [][2]string) ([]string, error) {
	var settings []string
	for _, p := range params {
		name, value := p[0], p[1]
		switch {
		case name == "user" || name == "database":
		case name == "options":
			opts, err := parseOptions(value)
			if err != nil {
				return nil, &PgError{Code: codeProtocolViolation, Message: err.Error()}
			}
			for _, o := range opts {
				settings = append(settings, setConfig(o[0], o[1]))
			}
		case name == "replication" || strings.HasPrefix(name, "_pq_."):
			return nil, &PgError{Code: codeFeatureNotSupported, Message: fmt.Sprintf("startup parameter %q is not supported by pglite", name)}
		default:
			settings = append(settings, setConfig(name, value))
		}
	}
	return settings, nil
}

func singleMessage(reply []byte) (message, bool) {
	var found message
	for m, err := range messages(reply) {
		if err != nil || found != nil {
			return nil, false
		}
		found = m
	}
	return found, found != nil
}

// describe summarises a reply for an error message.
func describe(reply []byte) string {
	if len(reply) == 0 {
		return "no reply"
	}
	var types []string
	for m, err := range messages(reply) {
		if err != nil {
			types = append(types, "malformed data")
			break
		}
		if m.typ() == 'E' {
			types = append(types, parseError(m.body()).Error())
		} else {
			types = append(types, string(m.typ()))
		}
	}
	return "messages " + strings.Join(types, ", ")
}

func setConfig(name, value string) string {
	return fmt.Sprintf("set_config(%s, %s, false)", quoteLiteral(name), quoteLiteral(value))
}

// parseOptions parses the "options" startup parameter: command-line
// switches of the form "-c name=value" or "--name=value", separated by
// spaces, with backslash escaping a space.
func parseOptions(s string) ([][2]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	var opts [][2]string
	for i := 0; i < len(words); i++ {
		var kv string
		switch w := words[i]; {
		case w == "-c" && i+1 < len(words):
			i++
			kv = words[i]
		case strings.HasPrefix(w, "-c") && len(w) > 2:
			kv = w[2:]
		case strings.HasPrefix(w, "--"):
			kv = w[2:]
		default:
			return nil, fmt.Errorf("unsupported option %q in startup parameter \"options\": pglite supports -c name=value and --name=value", w)
		}
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid option %q in startup parameter \"options\"", kv)
		}
		opts = append(opts, [2]string{strings.ReplaceAll(name, "-", "_"), value})
	}
	return opts, nil
}

// end returns the backend to the state of a new connection, also when the
// server is closing, so that the shutdown finds no open transaction:
//
//   - extended-protocol work the client left without a Sync is rolled
//     back: ROLLBACK aborts an implicit transaction block, and is skipped
//     when the backend is already discarding messages after an error; the
//     Sync then ends the pipeline;
//   - an open transaction block is rolled back;
//   - DISCARD ALL closes portals and cursors, drops prepared statements,
//     temporary tables and plans, releases session advisory locks, stops
//     listening and resets every setting to its default;
//   - the unnamed prepared statement, which DISCARD ALL keeps, is closed.
//
// If a step fails the backend is marked failed and refuses later sessions.
func (c *session) end() {
	b := c.server.backend
	if b.err() != nil {
		return
	}
	if c.unsynced {
		for _, m := range []message{queryMessage("ROLLBACK"), syncMessage} {
			reply, err := b.exec(m)
			if err != nil {
				return
			}
			c.noteStatus(reply)
		}
	}
	var steps []message
	if c.txStatus != 'I' {
		steps = append(steps, queryMessage("ROLLBACK"))
	}
	steps = append(steps, queryMessage("DISCARD ALL"), append(append(message(nil), closeUnnamedStatement...), syncMessage...))
	for _, m := range steps {
		reply, err := b.exec(m)
		if err != nil {
			return // the backend recorded the failure
		}
		for m, err := range messages(reply) {
			if err == nil && m.typ() == 'E' {
				err = parseError(m.body())
			}
			if err != nil {
				b.fail(fmt.Errorf("reset after session: %w", err))
				return
			}
		}
	}
}
