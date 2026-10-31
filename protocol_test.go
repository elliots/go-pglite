package pglite

import (
	"bufio"
	"bytes"
	"testing"
)

func TestParseStartupParams(t *testing.T) {
	params, err := parseStartupParams([]byte("user\x00postgres\x00database\x00postgres\x00\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(params) != 2 || params[0] != [2]string{"user", "postgres"} || params[1] != [2]string{"database", "postgres"} {
		t.Errorf("params = %v", params)
	}
	for _, bad := range []string{"user\x00postgres", "user\x00postgres\x00\x00extra", "user\x00"} {
		if _, err := parseStartupParams([]byte(bad)); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestReadMessageRejectsInvalidLength(t *testing.T) {
	for _, bad := range [][]byte{{'Q', 0, 0, 0, 1, 'x'}, {'Q', 0xff, 0xff, 0xff, 0xff}} {
		if _, err := readMessage(bufio.NewReader(bytes.NewReader(bad))); err == nil {
			t.Errorf("%v: want error", bad)
		}
	}
	if _, _, err := readStartup(bufio.NewReader(bytes.NewReader([]byte{0, 0, 0, 2, 0, 0, 0, 0}))); err == nil {
		t.Error("startup length 2: want error")
	}
}

func TestMessagesReportsTruncation(t *testing.T) {
	data := append(queryMessage("SELECT 1"), 'Z', 0, 0, 0)
	var got []byte
	var gotErr error
	for m, err := range messages(data) {
		if err != nil {
			gotErr = err
			break
		}
		got = append(got, m.typ())
	}
	if string(got) != "Q" || gotErr == nil {
		t.Errorf("types %q, err %v", got, gotErr)
	}
}

func TestParseDataRow(t *testing.T) {
	body := []byte{0, 2, 0, 0, 0, 2, 'h', 'i', 0xff, 0xff, 0xff, 0xff}
	row, err := parseDataRow(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(row) != 2 || row[0] == nil || *row[0] != "hi" || row[1] != nil {
		t.Errorf("row = %v", row)
	}
	if _, err := parseDataRow(body[:9]); err == nil {
		t.Error("truncated value: want error")
	}
}

func TestCopyFromStdinCount(t *testing.T) {
	cases := map[string]int{
		"COPY t FROM STDIN":                                                    1,
		"copy t (a, b) from stdin with (format csv);":                          1,
		"COPY \"from\" FROM /* stdin */ STDIN":                                 1,
		"-- COPY t FROM STDIN\nCOPY t FROM STDIN":                              1,
		"COPY t FROM\n\tSTDIN":                                                 1,
		"COPY t FROM STDIN; SELECT 1":                                          1,
		"BEGIN; COPY t FROM STDIN; COMMIT":                                     1,
		"COPY a FROM STDIN; COPY b FROM STDIN":                                 2,
		"COPY t TO STDOUT":                                                     0,
		"COPY t FROM '/tmp/file'":                                              0,
		"SELECT 'COPY t FROM STDIN'":                                           0,
		"SELECT 1; SELECT 'a; COPY t FROM STDIN'":                              0,
		"COPY (SELECT 1 FROM stdin) TO STDOUT":                                 0,
		"INSERT INTO t SELECT * FROM stdin":                                    0,
		"COPY t FROM PROGRAM 'cat'":                                            0,
		"CREATE FUNCTION f() RETURNS void AS 'COPY t FROM STDIN' LANGUAGE sql": 0,
		"": 0,
	}
	for q, want := range cases {
		if got := copyFromStdinCount(q); got != want {
			t.Errorf("%q: %d COPY FROM STDIN statements, want %d", q, got, want)
		}
	}
}

func TestSQLTokensSkipsLiteralsAndComments(t *testing.T) {
	toks := sqlTokens(`SELECT 'it''s', E'a\'b', "quoted ""id""", $$dollar from stdin$$, $t$x$t$ /* nested /* comment */ */ -- tail
	FROM t;`)
	want := []string{"select", "?", ",", "?", ",", "?", ",", "?", ",", "?", "from", "t"}
	if len(toks) != len(want) {
		t.Fatalf("tokens = %q, want %q", toks, want)
	}
	for i := range want {
		if toks[i] != want[i] {
			t.Errorf("token %d = %q, want %q", i, toks[i], want[i])
		}
	}
}

func TestParseOptions(t *testing.T) {
	opts, err := parseOptions(`-c search_path=public --work-mem=8MB -capplication_name=a\ b`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"search_path", "public"}, {"work_mem", "8MB"}, {"application_name", "a b"}}
	if len(opts) != len(want) {
		t.Fatalf("opts = %v", opts)
	}
	for i := range want {
		if opts[i] != want[i] {
			t.Errorf("option %d = %v, want %v", i, opts[i], want[i])
		}
	}
	for _, bad := range []string{"-x", "-c nothing", "--=v"} {
		if _, err := parseOptions(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestErrorResponseRoundTrip(t *testing.T) {
	m := errorResponse("FATAL", codeTooManyConnections, "busy")
	e := parseError(m.body())
	if e.Severity != "FATAL" || e.Code != codeTooManyConnections || e.Message != "busy" {
		t.Errorf("parsed %+v", e)
	}
}
