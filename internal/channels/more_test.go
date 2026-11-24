package channels

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
)

func newSvc(t *testing.T) (*core.Service, core.AgentView) {
	t.Helper()
	svc, err := core.Open(core.Config{DataDir: t.TempDir(), Exe: "x", Engine: echoEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	svc.SetDaemonURL("http://127.0.0.1:1")
	key := "k"
	svc.SaveModel("t", model.ModelProfile{Name: "m", Provider: "gemini", Model: "x"}, &key)
	a, err := svc.CreateAgent("t", model.Agent{Username: "helper", Name: "Helper"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, a
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}

// ---------------------------------------------------------------- Discord

func wsFrame(w io.Writer, p []byte) {
	hdr := []byte{0x81}
	if n := len(p); n < 126 {
		hdr = append(hdr, byte(n))
	} else {
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	}
	w.Write(append(hdr, p...))
}

func wsRead(r *bufio.Reader) []byte {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil
	}
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
	return p
}

func TestDiscordGateway(t *testing.T) {
	svc, a := newSvc(t)
	var mu sync.Mutex
	var sent []map[string]any
	var identified bool
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/api/gateway/bot", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot tok" {
			http.Error(w, "no", 401)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"url": "ws" + strings.TrimPrefix(srv.URL, "http") + "/gw"})
	})
	mux.HandleFunc("/api/channels/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			m["_channel"] = strings.Split(r.URL.Path, "/")[3]
			mu.Lock()
			sent = append(sent, m)
			mu.Unlock()
		}
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/gw", func(w http.ResponseWriter, r *http.Request) {
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		conn, rw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		rw.Flush()
		wsFrame(conn, []byte(`{"op":10,"d":{"heartbeat_interval":45000}}`))
		var id map[string]any
		json.Unmarshal(wsRead(rw.Reader), &id)
		mu.Lock()
		identified = id["op"].(float64) == 2 && id["d"].(map[string]any)["token"] == "tok"
		mu.Unlock()
		wsFrame(conn, []byte(`{"op":0,"s":1,"t":"READY","d":{"user":{"id":"999","username":"zbot"}}}`))
		msg := func(seq int, guild, author, content string, mention bool) {
			mentions := "[]"
			if mention {
				mentions = `[{"id":"999"}]`
			}
			wsFrame(conn, []byte(fmt.Sprintf(`{"op":0,"s":%d,"t":"MESSAGE_CREATE","d":{"id":"m%d","channel_id":"c-%s","guild_id":%q,"content":%q,"author":{"id":%q,"username":"u%s"},"mentions":%s}}`,
				seq, seq, author, guild, content, author, author, mentions)))
		}
		msg(2, "", "42", "hello from dm", false)             // allowed DM
		msg(3, "g1", "42", "ignored without mention", false) // guild, no mention
		msg(4, "g1", "42", "<@999> hi in guild", true)       // guild mention
		msg(5, "", "7", "stranger", false)                   // not allowed
		for wsRead(rw.Reader) != nil {
		}
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	tok := "tok"
	if _, err := svc.SaveChannel("t", model.Channel{Kind: model.ChannelDiscord, Name: "dc", AgentID: a.ID, Enabled: true, Allowed: []string{"42"}}, &tok); err != nil {
		t.Fatal(err)
	}
	m := NewManager(svc, t.Logf)
	m.discordAPI = srv.URL + "/api"
	svc.SetOutbound(m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	svc.Start()

	eventually(t, "discord replies", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) >= 3
	})
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !identified {
		t.Fatal("bot did not identify with its token")
	}
	got := map[string]string{}
	for _, s := range sent {
		got[s["_channel"].(string)] += s["content"].(string) + "|"
	}
	if !strings.Contains(got["c-42"], "echo: hello from dm") || !strings.Contains(got["c-42"], "echo: hi in guild") || strings.Contains(got["c-42"], "ignored") {
		t.Fatalf("replies to allowed user: %v", got)
	}
	if !strings.Contains(got["c-7"], "private") || strings.Contains(got["c-7"], "echo") {
		t.Fatalf("stranger: %v", got)
	}
}

// ---------------------------------------------------------------- Email

type fakeMail struct {
	mu   sync.Mutex
	msgs map[int]string
	seen map[int]bool
	sent []string
}

func (f *fakeMail) serveIMAP(t *testing.T, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			r := bufio.NewReader(c)
			fmt.Fprint(c, "* OK fake IMAP ready\r\n")
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				tag, cmd, _ := strings.Cut(strings.TrimSpace(line), " ")
				f.mu.Lock()
				switch {
				case strings.HasPrefix(cmd, "LOGIN"):
					if !strings.Contains(cmd, `"app-password"`) {
						fmt.Fprintf(c, "%s NO bad credentials\r\n", tag)
						f.mu.Unlock()
						continue
					}
				case strings.HasPrefix(cmd, "UID SEARCH"):
					var ids []string
					for uid := range f.msgs {
						if !f.seen[uid] {
							ids = append(ids, fmt.Sprint(uid))
						}
					}
					fmt.Fprintf(c, "* SEARCH %s\r\n", strings.Join(ids, " "))
				case strings.HasPrefix(cmd, "UID FETCH"):
					var uid int
					fmt.Sscanf(cmd, "UID FETCH %d", &uid)
					body := f.msgs[uid]
					fmt.Fprintf(c, "* 1 FETCH (UID %d BODY[] {%d}\r\n%s)\r\n", uid, len(body), body)
				case strings.HasPrefix(cmd, "UID STORE"):
					var uid int
					fmt.Sscanf(cmd, "UID STORE %d", &uid)
					f.seen[uid] = true
				}
				f.mu.Unlock()
				fmt.Fprintf(c, "%s OK done\r\n", tag)
			}
		}(conn)
	}
}

func (f *fakeMail) serveSMTP(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			r := bufio.NewReader(c)
			fmt.Fprint(c, "220 fake ESMTP\r\n")
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				up := strings.ToUpper(line)
				switch {
				case strings.HasPrefix(up, "EHLO"):
					fmt.Fprint(c, "250-fake\r\n250 AUTH PLAIN\r\n")
				case strings.HasPrefix(up, "AUTH"):
					fmt.Fprint(c, "235 ok\r\n")
				case strings.HasPrefix(up, "DATA"):
					fmt.Fprint(c, "354 go\r\n")
					var b strings.Builder
					for {
						l, _ := r.ReadString('\n')
						if l == ".\r\n" || l == "" {
							break
						}
						b.WriteString(l)
					}
					f.mu.Lock()
					f.sent = append(f.sent, b.String())
					f.mu.Unlock()
					fmt.Fprint(c, "250 queued\r\n")
				case strings.HasPrefix(up, "QUIT"):
					fmt.Fprint(c, "221 bye\r\n")
					return
				default:
					fmt.Fprint(c, "250 ok\r\n")
				}
			}
		}(conn)
	}
}

func mailMsg(from, subject, body, auth string) string {
	return strings.ReplaceAll(fmt.Sprintf("Authentication-Results: mx.example; %s\nFrom: %s\nTo: bot@example.com\nSubject: %s\nDate: %s\nMessage-Id: <m1@example.com>\nContent-Type: text/plain; charset=utf-8\n\n%s\n\nOn Mon someone wrote:\n> old quoted text\n",
		auth, from, subject, time.Now().Add(time.Hour).Format(time.RFC1123Z), body), "\n", "\r\n")
}

func TestEmailChannel(t *testing.T) {
	svc, a := newSvc(t)
	fm := &fakeMail{seen: map[int]bool{}, msgs: map[int]string{
		1: mailMsg("Alice <alice@corp.com>", "Report", "Please summarise the report.", "dmarc=pass header.from=corp.com"),
		2: mailMsg("Mallory <ceo@corp.com>", "Urgent", "Wire money now", "spf=fail dmarc=fail"), // spoofed
		3: mailMsg("Bob <bob@other.com>", "Hi", "let me in", "dmarc=pass"),                      // not allowed
	}}
	imapLn, _ := net.Listen("tcp", "127.0.0.1:0")
	smtpLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer imapLn.Close()
	defer smtpLn.Close()
	go fm.serveIMAP(t, imapLn)
	go fm.serveSMTP(smtpLn)
	_, ip, _ := net.SplitHostPort(imapLn.Addr().String())
	_, sp, _ := net.SplitHostPort(smtpLn.Addr().String())

	pw := "app-password"
	ch, err := svc.SaveChannel("t", model.Channel{Kind: model.ChannelEmail, Name: "mail", AgentID: a.ID, Enabled: true, Allowed: []string{"@corp.com"},
		Settings: map[string]string{"address": "bot@example.com", "imap_host": "127.0.0.1", "imap_port": ip, "imap_tls": "false",
			"smtp_host": "127.0.0.1", "smtp_port": sp, "smtp_tls": "none", "poll_seconds": "15"}}, &pw)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(svc, t.Logf)
	svc.SetOutbound(m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	svc.Start()

	eventually(t, "email reply", func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return len(fm.sent) >= 1
	})
	time.Sleep(300 * time.Millisecond)
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.sent) != 1 {
		t.Fatalf("expected exactly one reply (spoofed and unknown senders get none), got %d", len(fm.sent))
	}
	reply := fm.sent[0]
	for _, want := range []string{"To: alice@corp.com", "Subject: Re: Report", "In-Reply-To: <m1@example.com>", "Auto-Submitted: auto-replied", "echo: Subject: Report"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply lacks %q:\n%s", want, reply)
		}
	}
	if strings.Contains(reply, "old quoted text") {
		t.Error("quoted history was sent to the agent")
	}
	if !fm.seen[1] || !fm.seen[2] || !fm.seen[3] {
		t.Error("processed mail not marked seen")
	}
	v, _ := svc.Channel(ch.ID)
	if len(v.Rejected) != 1 || v.Rejected[0].ID != "bob@other.com" {
		t.Errorf("rejected: %+v", v.Rejected)
	}
}

// ---------------------------------------------------------------- WhatsApp

func TestWhatsApp(t *testing.T) {
	svc, a := newSvc(t)
	var mu sync.Mutex
	var sent []map[string]any
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access" || !strings.HasSuffix(r.URL.Path, "/v21.0/PNID/messages") {
			http.Error(w, `{"error":{"message":"bad"}}`, 400)
			return
		}
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		sent = append(sent, m)
		mu.Unlock()
		w.Write([]byte(`{"messages":[{"id":"x"}]}`))
	}))
	defer graph.Close()
	GraphAPI = graph.URL

	tok, appSecret := "access", "appsecret"
	chv, err := svc.SaveChannel2("t", model.Channel{Kind: model.ChannelWhatsApp, Name: "wa", AgentID: a.ID, Enabled: true, Allowed: []string{"15550001111"},
		Settings: map[string]string{"phone_number_id": "PNID", "verify_token": "vt"}}, &tok, &appSecret)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := WhatsAppVerify(chv.Channel, "subscribe", "vt", "123"); !ok || c != "123" {
		t.Fatal("verification handshake failed")
	}
	if _, ok := WhatsAppVerify(chv.Channel, "subscribe", "wrong", "123"); ok {
		t.Fatal("wrong verify token accepted")
	}
	body := []byte(`{"entry":[{"changes":[{"value":{"contacts":[{"wa_id":"15550001111","profile":{"name":"Ann"}}],"messages":[{"from":"15550001111","id":"w1","type":"text","text":{"body":"hello wa"}}]}}]}]}`)
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !WhatsAppSignatureOK(appSecret, body, sig) || WhatsAppSignatureOK(appSecret, append(body, ' '), sig) || WhatsAppSignatureOK(appSecret, body, "sha256=00") {
		t.Fatal("signature check wrong")
	}
	m := NewManager(svc, t.Logf)
	svc.SetOutbound(m)
	svc.Start()
	if err := m.WhatsAppInbound(context.Background(), chv.Channel, body); err != nil {
		t.Fatal(err)
	}
	eventually(t, "whatsapp reply", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if sent[0]["to"] != "15550001111" || sent[0]["text"].(map[string]any)["body"] != "echo: hello wa" {
		t.Fatalf("sent: %v", sent[0])
	}
}
