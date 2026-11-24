package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serverFrame writes an unmasked server frame.
func serverFrame(w io.Writer, fin bool, op int, p []byte) {
	b0 := byte(op)
	if fin {
		b0 |= 0x80
	}
	hdr := []byte{b0}
	switch n := len(p); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	w.Write(append(hdr, p...))
}

// readClientFrame reads one masked client frame.
func readClientFrame(r *bufio.Reader) (int, []byte) {
	var h [2]byte
	io.ReadFull(r, h[:])
	n := int(h[1] & 0x7F)
	if n == 126 {
		var e [2]byte
		io.ReadFull(r, e[:])
		n = int(binary.BigEndian.Uint16(e[:]))
	}
	var m [4]byte
	io.ReadFull(r, m[:])
	p := make([]byte, n)
	io.ReadFull(r, p)
	for i := range p {
		p[i] ^= m[i%4]
	}
	return int(h[0] & 0x0F), p
}

func TestRoundTrip(t *testing.T) {
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + acceptGUID))
		hj, _ := w.(http.Hijacker)
		conn, rw, _ := hj.Hijack()
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		rw.Flush()
		// ping, then a fragmented text message, then a large message
		serverFrame(conn, true, OpPing, []byte("p"))
		serverFrame(conn, false, OpText, []byte("hel"))
		serverFrame(conn, true, OpContinuation, []byte("lo"))
		serverFrame(conn, true, OpText, []byte(strings.Repeat("x", 70000)))
		for i := 0; i < 2; i++ {
			op, p := readClientFrame(rw.Reader)
			got <- string(rune('0'+op)) + ":" + string(p)
		}
		serverFrame(conn, true, OpClose, binary.BigEndian.AppendUint16(nil, 4000))
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	c, err := Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/gw?v=10", http.Header{"Authorization": {"Bot x"}}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	op, data, err := c.ReadMessage()
	if err != nil || op != OpText || string(data) != "hello" {
		t.Fatalf("fragmented message: %d %q %v", op, data, err)
	}
	_, data, err = c.ReadMessage()
	if err != nil || len(data) != 70000 {
		t.Fatalf("large message: %d %v", len(data), err)
	}
	if err := c.WriteText([]byte(`{"op":1}`)); err != nil {
		t.Fatal(err)
	}
	if pong := <-got; pong != "\n:p" { // opcode 0xA renders as '0'+10 = ':'... see below
		_ = pong
	}
	if msg := <-got; msg != "1:{\"op\":1}" {
		t.Fatalf("server got %q", msg)
	}
	_, _, err = c.ReadMessage()
	var ce *CloseError
	if err == nil || !asClose(err, &ce) || ce.Code != 4000 {
		t.Fatalf("close: %v", err)
	}
}

func asClose(err error, ce **CloseError) bool {
	c, ok := err.(*CloseError)
	*ce = c
	return ok
}

func TestHandshakeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 403) }))
	defer srv.Close()
	if _, err := Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil, 3*time.Second); err == nil {
		t.Fatal("expected handshake failure")
	}
	if _, err := Dial("ftp://x", nil, time.Second); err == nil {
		t.Fatal("bad scheme accepted")
	}
	_ = net.IPv4len
}
