package pglite

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// startupMessage returns a StartupMessage (length-prefixed, no type byte)
// with the given protocol code and body.
func startupMessage(code uint32, body []byte) []byte {
	msg := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(msg, uint32(len(msg)))
	binary.BigEndian.PutUint32(msg[4:], code)
	copy(msg[8:], body)
	return msg
}

// message returns a typed message: type byte, int32 length, body.
func message(typ byte, body []byte) []byte {
	msg := make([]byte, 5+len(body))
	msg[0] = typ
	binary.BigEndian.PutUint32(msg[1:], uint32(4+len(body)))
	copy(msg[5:], body)
	return msg
}

func TestSplitMessagesHoldsBackIncompleteMessage(t *testing.T) {
	q := message('Q', []byte("SELECT 1\x00"))
	complete, rest, startup, err := splitMessages(q[:7], false)
	if err != nil {
		t.Fatal(err)
	}
	if len(complete) != 0 || !bytes.Equal(rest, q[:7]) || startup {
		t.Errorf("partial message: complete=%d rest=%d startup=%v", len(complete), len(rest), startup)
	}
}

func TestSplitMessagesReturnsAllCompleteMessages(t *testing.T) {
	q1 := message('Q', []byte("SELECT 1\x00"))
	q2 := message('Q', []byte("SELECT 2\x00"))
	q3 := message('Q', []byte("SELECT 3\x00"))
	data := append(append(append([]byte{}, q1...), q2...), q3[:3]...)
	complete, rest, _, err := splitMessages(data, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(complete, append(append([]byte{}, q1...), q2...)) {
		t.Errorf("complete = %q", complete)
	}
	if !bytes.Equal(rest, q3[:3]) {
		t.Errorf("rest = %q", rest)
	}
}

func TestSplitMessagesStartupOneAtATime(t *testing.T) {
	ssl := startupMessage(80877103, nil)
	start := startupMessage(196608, []byte("user\x00postgres\x00\x00"))
	data := append(append([]byte{}, ssl...), start...)

	complete, rest, startup, err := splitMessages(data, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(complete, ssl) || !startup {
		t.Fatalf("SSLRequest: complete=%q startup=%v", complete, startup)
	}
	complete, rest, startup, err = splitMessages(rest, startup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(complete, start) || startup || len(rest) != 0 {
		t.Errorf("StartupMessage: complete=%q startup=%v rest=%d", complete, startup, len(rest))
	}
}

func TestSplitMessagesRejectsInvalidLength(t *testing.T) {
	bad := []byte{'Q', 0, 0, 0, 1, 'x'}
	if _, _, _, err := splitMessages(bad, false); err == nil {
		t.Error("length 1: want error")
	}
	if _, _, _, err := splitMessages([]byte{0, 0, 0, 2}, true); err == nil {
		t.Error("startup length 2: want error")
	}
}
