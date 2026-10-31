package pglite

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
)

// PostgreSQL wire protocol (version 3.0) helpers.

const (
	protocolVersion3 = 196608   // 3.0
	cancelRequest    = 80877102 // CancelRequest
	sslRequest       = 80877103 // SSLRequest
	gssEncRequest    = 80877104 // GSSENCRequest

	// maxMessageLen bounds a frontend message, as PostgreSQL's
	// PQ_LARGE_MESSAGE_LIMIT does.
	maxMessageLen = 1<<30 - 1
	// maxStartupLen bounds a startup packet, as PostgreSQL's
	// MAX_STARTUP_PACKET_LENGTH does.
	maxStartupLen = 10000
)

// message is one protocol message including its type byte and length.
type message []byte

func (m message) typ() byte    { return m[0] }
func (m message) body() []byte { return m[5:] }

// readMessage reads one typed message.
func readMessage(r *bufio.Reader) (message, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n < 4 || n > maxMessageLen {
		return nil, fmt.Errorf("invalid length %d for message type %q", n, hdr[0])
	}
	m := make(message, 1+n)
	copy(m, hdr[:])
	if _, err := io.ReadFull(r, m[5:]); err != nil {
		return nil, err
	}
	return m, nil
}

// readStartup reads an untyped startup-phase packet and returns its
// request code and the bytes after it.
func readStartup(r *bufio.Reader) (uint32, []byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n < 8 || n > maxStartupLen {
		return 0, nil, fmt.Errorf("invalid startup packet length %d", n)
	}
	rest := make([]byte, n-8)
	if _, err := io.ReadFull(r, rest); err != nil {
		return 0, nil, err
	}
	return binary.BigEndian.Uint32(hdr[4:]), rest, nil
}

// parseStartupParams parses the name/value pairs of a StartupMessage.
func parseStartupParams(b []byte) ([][2]string, error) {
	var params [][2]string
	for {
		name, rest, ok := bytes.Cut(b, []byte{0})
		if !ok {
			return nil, errors.New("startup packet is not NUL-terminated")
		}
		if len(name) == 0 {
			if len(rest) != 0 {
				return nil, errors.New("startup packet has data after its terminator")
			}
			return params, nil
		}
		value, rest, ok := bytes.Cut(rest, []byte{0})
		if !ok {
			return nil, fmt.Errorf("startup parameter %q has no value", name)
		}
		params = append(params, [2]string{string(name), string(value)})
		b = rest
	}
}

// builder assembles protocol messages.
type builder struct{ buf []byte }

// begin starts a typed message and returns the offset of its length field.
func (b *builder) begin(typ byte) int {
	b.buf = append(b.buf, typ, 0, 0, 0, 0)
	return len(b.buf) - 4
}

// end fills in the length of the message begun at lenAt.
func (b *builder) end(lenAt int) {
	binary.BigEndian.PutUint32(b.buf[lenAt:], uint32(len(b.buf)-lenAt))
}

func (b *builder) int32(v uint32) { b.buf = binary.BigEndian.AppendUint32(b.buf, v) }
func (b *builder) cstring(s string) {
	b.buf = append(b.buf, s...)
	b.buf = append(b.buf, 0)
}

// typedMessage returns a message of the given type with a body.
func typedMessage(typ byte, body ...func(*builder)) message {
	var b builder
	s := b.begin(typ)
	for _, f := range body {
		f(&b)
	}
	b.end(s)
	return b.buf
}

// queryMessage returns a simple-protocol Query message.
func queryMessage(sql string) message {
	return typedMessage('Q', func(b *builder) { b.cstring(sql) })
}

// passwordMessage returns a PasswordMessage.
func passwordMessage(password string) message {
	return typedMessage('p', func(b *builder) { b.cstring(password) })
}

// syncMessage is the extended-protocol Sync message.
var syncMessage = message{'S', 0, 0, 0, 4}

// closeUnnamedStatement is Close('S', "").
var closeUnnamedStatement = message{'C', 0, 0, 0, 6, 'S', 0}

// startupMessage returns a protocol 3.0 StartupMessage with the given
// parameter names and values, alternating.
func startupMessage(params ...string) []byte {
	var b builder
	b.buf = append(b.buf, 0, 0, 0, 0)
	b.int32(protocolVersion3)
	for _, p := range params {
		b.cstring(p)
	}
	b.buf = append(b.buf, 0)
	binary.BigEndian.PutUint32(b.buf, uint32(len(b.buf)))
	return b.buf
}

// SQLSTATE codes of the errors the server reports itself.
const (
	codeTooManyConnections  = "53300"
	codeInvalidDatabase     = "3D000"
	codeInvalidAuthSpec     = "28000"
	codeProtocolViolation   = "08P01"
	codeFeatureNotSupported = "0A000"
	codeAdminShutdown       = "57P01"
	codeInternalError       = "XX000"
)

// readyForQuery returns a ReadyForQuery message with a transaction status.
func readyForQuery(txStatus byte) message {
	return message{'Z', 0, 0, 0, 5, txStatus}
}

// errorResponse returns an ErrorResponse message.
func errorResponse(severity, code, msg string) message {
	return typedMessage('E', func(b *builder) {
		for _, f := range [][2]string{{"S", severity}, {"V", severity}, {"C", code}, {"M", msg}} {
			b.buf = append(b.buf, f[0][0])
			b.cstring(f[1])
		}
		b.buf = append(b.buf, 0)
	})
}

// messages iterates over the complete messages in a byte stream, for
// example a backend reply. It ends with an error if the data ends inside a
// message.
func messages(data []byte) iter.Seq2[message, error] {
	return func(yield func(message, error) bool) {
		for len(data) > 0 {
			if len(data) < 5 {
				yield(nil, fmt.Errorf("truncated message header (%d bytes)", len(data)))
				return
			}
			n := int(binary.BigEndian.Uint32(data[1:]))
			if n < 4 || 1+n > len(data) {
				yield(nil, fmt.Errorf("message %q has length %d with %d bytes left", data[0], n, len(data)-1))
				return
			}
			if !yield(message(data[:1+n]), nil) {
				return
			}
			data = data[1+n:]
		}
	}
}

// PgError is an error PostgreSQL reported to the bridge's own queries.
type PgError struct {
	Severity string
	Code     string
	Message  string
	Detail   string
}

func (e *PgError) Error() string {
	s := fmt.Sprintf("%s: %s (SQLSTATE %s)", e.Severity, e.Message, e.Code)
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// parseError decodes the body of an ErrorResponse or NoticeResponse.
func parseError(body []byte) *PgError {
	e := &PgError{}
	for len(body) > 0 && body[0] != 0 {
		field := body[0]
		value, rest, _ := bytes.Cut(body[1:], []byte{0})
		body = rest
		switch field {
		case 'V':
			e.Severity = string(value)
		case 'S':
			if e.Severity == "" {
				e.Severity = string(value)
			}
		case 'C':
			e.Code = string(value)
		case 'M':
			e.Message = string(value)
		case 'D':
			e.Detail = string(value)
		}
	}
	return e
}

// parseDataRow decodes a DataRow in text format. NULL becomes nil.
func parseDataRow(body []byte) ([]*string, error) {
	if len(body) < 2 {
		return nil, errors.New("truncated DataRow")
	}
	n := int(binary.BigEndian.Uint16(body))
	body = body[2:]
	row := make([]*string, n)
	for i := range n {
		if len(body) < 4 {
			return nil, errors.New("truncated DataRow")
		}
		l := int32(binary.BigEndian.Uint32(body))
		body = body[4:]
		if l < 0 {
			continue
		}
		if int(l) > len(body) {
			return nil, errors.New("truncated DataRow value")
		}
		row[i] = new(string(body[:l]))
		body = body[l:]
	}
	return row, nil
}

// copyFromStdinCount returns how many of the statements in sql are COPY ...
// FROM STDIN. Such a statement needs its CopyData stream in the same call
// into the backend (see session.copyIn): the backend treats the end of its
// input during a COPY as a lost connection and cannot be used afterwards.
// The SQL is tokenised so that keywords inside literals, quoted identifiers
// and comments do not count.
func copyFromStdinCount(sql string) int {
	count := 0
	for _, stmt := range splitStatements(sqlTokens(sql)) {
		if len(stmt) < 3 || stmt[0] != "copy" {
			continue
		}
		depth := 0
		for i, t := range stmt {
			switch t {
			case "(":
				depth++
			case ")":
				depth--
			case "from":
				if depth == 0 && i+1 < len(stmt) && stmt[i+1] == "stdin" {
					count++
				}
			}
		}
	}
	return count
}

// splitStatements splits a token list at the semicolons outside
// parentheses.
func splitStatements(toks []string) [][]string {
	var stmts [][]string
	start, depth := 0, 0
	for i, t := range toks {
		switch t {
		case "(":
			depth++
		case ")":
			depth--
		case ";":
			if depth == 0 {
				stmts = append(stmts, toks[start:i])
				start = i + 1
			}
		}
	}
	return append(stmts, toks[start:])
}

// sqlTokens splits SQL into lower-cased words and punctuation, dropping
// whitespace and comments; a string literal or quoted identifier becomes
// the single token "?". A trailing semicolon is dropped.
func sqlTokens(sql string) []string {
	var toks []string
	s := sql
	for len(s) > 0 {
		c := s[0]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			s = s[1:]
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
			} else {
				s = ""
			}
		case strings.HasPrefix(s, "/*"):
			depth, i := 0, 0
			for i < len(s) {
				if strings.HasPrefix(s[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(s[i:], "*/") {
					depth--
					i += 2
					if depth == 0 {
						break
					}
				} else {
					i++
				}
			}
			s = s[i:]
		case c == '\'' || c == '"':
			s = skipQuoted(s, c, false)
			toks = append(toks, "?")
		case (c == 'e' || c == 'E') && len(s) > 1 && s[1] == '\'':
			s = skipQuoted(s[1:], '\'', true)
			toks = append(toks, "?")
		case c == '$':
			if tag, ok := dollarTag(s); ok {
				if i := strings.Index(s[len(tag):], tag); i >= 0 {
					s = s[len(tag)+i+len(tag):]
				} else {
					s = ""
				}
				toks = append(toks, "?")
			} else {
				toks, s = append(toks, "$"), s[1:]
			}
		case isWordByte(c):
			i := 0
			for i < len(s) && isWordByte(s[i]) {
				i++
			}
			toks, s = append(toks, strings.ToLower(s[:i])), s[i:]
		default:
			toks, s = append(toks, s[:1]), s[1:]
		}
	}
	if n := len(toks); n > 0 && toks[n-1] == ";" {
		toks = toks[:n-1]
	}
	return toks
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 0x80 || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// skipQuoted skips a quoted literal or identifier starting at s[0] == q.
// Doubled quotes are escapes; backslash escapes apply to E” strings.
func skipQuoted(s string, q byte, backslash bool) string {
	for i := 1; i < len(s); i++ {
		switch {
		case backslash && s[i] == '\\':
			i++
		case s[i] == q:
			if i+1 < len(s) && s[i+1] == q {
				i++
				continue
			}
			return s[i+1:]
		}
	}
	return ""
}

// dollarTag returns the opening tag of a dollar-quoted string ($$ or $tag$).
func dollarTag(s string) (string, bool) {
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '$':
			return s[:i+1], true
		case c == '_' || c >= 0x80 || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || (i > 1 && '0' <= c && c <= '9'):
		default:
			return "", false
		}
	}
	return "", false
}

// quoteLiteral returns s as a SQL string literal.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
