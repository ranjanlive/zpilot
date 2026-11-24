// Package ws is a small RFC 6455 WebSocket client: enough for chat gateways
// (Discord) without a third-party dependency. It supports TLS, text and
// binary messages, fragmentation, ping/pong and close; it does not implement
// extensions (no permessage-deflate).
package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Opcodes.
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// MaxMessage caps a reassembled message.
const MaxMessage = 16 << 20

const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Conn is a client connection. Reads must come from one goroutine; writes are
// safe from any goroutine.
type Conn struct {
	c   net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
}

// CloseError is returned by ReadMessage when the server closes the connection.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string { return fmt.Sprintf("websocket closed (%d) %s", e.Code, e.Reason) }

// Dial connects to a ws:// or wss:// URL.
func Dial(rawURL string, header http.Header, timeout time.Duration) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	d := &net.Dialer{Timeout: timeout}
	var nc net.Conn
	switch u.Scheme {
	case "wss":
		nc, err = tls.DialWithDialer(d, "tcp", host, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	case "ws":
		nc, err = d.Dial("tcp", host)
	default:
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", path, u.Host, key)
	for k, vs := range header {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	_ = nc.SetDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(nc, b.String()); err != nil {
		nc.Close()
		return nil, err
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		nc.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		nc.Close()
		return nil, fmt.Errorf("websocket handshake: HTTP %d", resp.StatusCode)
	}
	sum := sha1.Sum([]byte(key + acceptGUID))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		nc.Close()
		return nil, errors.New("websocket handshake: bad Sec-WebSocket-Accept")
	}
	_ = nc.SetDeadline(time.Time{})
	return &Conn{c: nc, br: br}, nil
}

// SetReadDeadline bounds the next read.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.c.SetReadDeadline(t) }

// ReadMessage returns the next text or binary message, answering pings.
func (c *Conn) ReadMessage() (op int, data []byte, err error) {
	var msgOp int
	var buf []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case OpPing:
			if err := c.writeFrame(OpPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case OpPong:
			continue
		case OpClose:
			ce := &CloseError{Code: 1005}
			if len(payload) >= 2 {
				ce.Code = int(binary.BigEndian.Uint16(payload))
				ce.Reason = string(payload[2:])
			}
			_ = c.writeFrame(OpClose, payload[:min(len(payload), 2)])
			c.c.Close()
			return 0, nil, ce
		case OpText, OpBinary:
			if msgOp != 0 {
				return 0, nil, errors.New("websocket: new message before the previous one finished")
			}
			msgOp = op
		case OpContinuation:
			if msgOp == 0 {
				return 0, nil, errors.New("websocket: unexpected continuation frame")
			}
		default:
			return 0, nil, fmt.Errorf("websocket: unknown opcode %d", op)
		}
		buf = append(buf, payload...)
		if len(buf) > MaxMessage {
			return 0, nil, errors.New("websocket: message too large")
		}
		if fin {
			return msgOp, buf, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op int, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	op = int(h[0] & 0x0F)
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if n > MaxMessage {
		err = errors.New("websocket: frame too large")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// WriteText sends a text message.
func (c *Conn) WriteText(data []byte) error { return c.writeFrame(OpText, data) }

// writeFrame sends one masked, final frame (clients must mask).
func (c *Conn) writeFrame(op int, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	hdr := []byte{0x80 | byte(op)}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_ = c.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.c.Write(append(hdr, masked...))
	return err
}

// Close sends a close frame and closes the connection.
func (c *Conn) Close(code int) error {
	_ = c.writeFrame(OpClose, binary.BigEndian.AppendUint16(nil, uint16(code)))
	return c.c.Close()
}
