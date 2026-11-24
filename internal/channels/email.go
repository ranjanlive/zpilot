package channels

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"regexp"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
)

// Email channel settings:
//
//	address        the mailbox address (also the From of replies)
//	imap_host      e.g. imap.gmail.com      imap_port (993)   imap_tls (true)
//	smtp_host      e.g. smtp.gmail.com      smtp_port (587)   smtp_tls: starttls | tls | none
//	username       login name (default: address)
//	poll_seconds   how often to check (default 60)
//	require_authenticated  "false" to accept mail without SPF/DKIM/DMARC pass (not recommended)
//
// The password (for Gmail: an app password) is the channel secret.

type mailRef struct {
	Subject   string
	MessageID string
}

func setting(ch model.Channel, k, def string) string {
	if v := strings.TrimSpace(ch.Settings[k]); v != "" {
		return v
	}
	return def
}

func (m *Manager) pollEmail(ctx context.Context, channelID, password string) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		chv, err := m.svc.Channel(channelID)
		if err != nil {
			return
		}
		ch := chv.Channel
		n, err := m.checkMailbox(ctx, ch, password)
		if err != nil {
			m.svc.SetChannelStatus(channelID, "error: "+err.Error())
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 10*time.Minute)
			continue
		}
		backoff = 5 * time.Second
		m.svc.SetChannelStatus(channelID, fmt.Sprintf("checked %s · %d new", time.Now().Format("15:04"), n))
		secs := 60
		fmt.Sscan(setting(ch, "poll_seconds", "60"), &secs)
		if !sleep(ctx, time.Duration(max(secs, 15))*time.Second) {
			return
		}
	}
}

// checkMailbox processes unseen mail once and returns how many were delivered.
func (m *Manager) checkMailbox(ctx context.Context, ch model.Channel, password string) (int, error) {
	addr := net.JoinHostPort(setting(ch, "imap_host", ""), setting(ch, "imap_port", "993"))
	c, err := dialIMAP(addr, setting(ch, "imap_tls", "true") != "false", 30*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.close()
	if err := c.login(setting(ch, "username", setting(ch, "address", "")), password); err != nil {
		return 0, fmt.Errorf("login failed (for Gmail use an app password): %w", err)
	}
	if _, err := c.cmd("SELECT INBOX"); err != nil {
		return 0, err
	}
	uids, err := c.searchUnseen(ch.Created)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, uid := range uids {
		if ctx.Err() != nil {
			break
		}
		raw, err := c.fetch(uid)
		if err != nil {
			return delivered, err
		}
		_ = c.markSeen(uid) // never process a message twice, even if it is rejected
		if m.handleEmail(ctx, ch, raw) {
			delivered++
		}
	}
	_, _ = c.cmd("LOGOUT")
	return delivered, nil
}

var quoteStart = regexp.MustCompile(`(?m)^(On .{0,200} wrote:|-----Original Message-----|________________________________)\s*$`)

// handleEmail parses one message and routes it. It reports whether the
// message reached an agent.
func (m *Manager) handleEmail(ctx context.Context, ch model.Channel, raw []byte) bool {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	from, err := mail.ParseAddress(decodeHeader(msg.Header.Get("From")))
	if err != nil {
		return false
	}
	sender := strings.ToLower(from.Address)
	self := strings.ToLower(setting(ch, "address", ""))
	if sender == self || msg.Header.Get("Auto-Submitted") != "" && msg.Header.Get("Auto-Submitted") != "no" || strings.Contains(strings.ToLower(msg.Header.Get("Precedence")), "bulk") {
		return false // our own mail, auto-replies and bulk mail would loop or spam
	}
	if d, err := msg.Header.Date(); err == nil && d.Before(ch.Created) {
		return false
	}
	if setting(ch, "require_authenticated", "true") != "false" && !mailAuthenticated(msg.Header, sender) {
		m.log("email %s: dropped mail claiming to be from %s: not authenticated (SPF/DKIM/DMARC)", ch.Name, sender)
		return false
	}
	subject := decodeHeader(msg.Header.Get("Subject"))
	body := strings.TrimSpace(textBody(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body))
	if loc := quoteStart.FindStringIndex(body); loc != nil {
		body = strings.TrimSpace(body[:loc[0]])
	}
	var kept []string
	for _, l := range strings.Split(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), ">") {
			kept = append(kept, l)
		}
	}
	body = strings.TrimSpace(strings.Join(kept, "\n"))
	if len(body) > 20000 {
		body = body[:20000] + "\n…(truncated)"
	}
	text := body
	if subject != "" {
		text = "Subject: " + subject + "\n\n" + body
	}
	if strings.TrimSpace(body) == "" && subject == "" {
		return false
	}
	m.mu.Lock()
	m.mail[ch.ID+"/"+sender] = mailRef{Subject: subject, MessageID: msg.Header.Get("Message-Id")}
	m.mu.Unlock()
	name := from.Name
	if name == "" {
		name = sender
	}
	_, allowed, err := m.svc.ChannelInbound(core.Inbound{ChannelID: ch.ID, ChatID: sender, SenderID: sender, SenderName: name, Text: text})
	return err == nil && allowed
}

// mailAuthenticated checks the receiving server's Authentication-Results
// for a DMARC pass, or a DKIM/SPF pass aligned with the sender's domain.
func mailAuthenticated(h mail.Header, sender string) bool {
	domain := sender[strings.LastIndex(sender, "@")+1:]
	for _, ar := range h["Authentication-Results"] {
		a := strings.ToLower(ar)
		if strings.Contains(a, "dmarc=pass") {
			return true
		}
		if strings.Contains(a, "dkim=pass") && strings.Contains(a, "header.d="+domain) {
			return true
		}
		if strings.Contains(a, "spf=pass") && strings.Contains(a, "@"+domain) {
			return true
		}
	}
	return false
}

func decodeHeader(s string) string {
	d, err := new(mime.WordDecoder).DecodeHeader(s)
	if err != nil {
		return s
	}
	return d
}

// textBody extracts the text/plain content (falling back to stripped HTML).
func textBody(contentType, encoding string, r io.Reader) string {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(r, params["boundary"])
		var html string
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			t := textBody(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p)
			pmt, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
			switch {
			case pmt == "text/plain" || strings.HasPrefix(pmt, "multipart/") && t != "":
				return t
			case pmt == "text/html" && html == "":
				html = t
			}
		}
		return html
	}
	var rd io.Reader = io.LimitReader(r, 2<<20)
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		rd = quotedprintable.NewReader(rd)
	case "base64":
		rd = base64.NewDecoder(base64.StdEncoding, newlineStripper{rd})
	}
	b, _ := io.ReadAll(rd)
	text := string(b)
	if mt == "text/html" {
		text = stripHTML(text)
	} else if mt != "text/plain" {
		return ""
	}
	return text
}

type newlineStripper struct{ r io.Reader }

func (n newlineStripper) Read(p []byte) (int, error) {
	k, err := n.r.Read(p)
	j := 0
	for _, c := range p[:k] {
		if c != '\r' && c != '\n' {
			p[j] = c
			j++
		}
	}
	return j, err
}

var (
	tagRe   = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]+>`)
	spaceRe = regexp.MustCompile(`\n{3,}`)
)

func stripHTML(s string) string {
	s = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n\n", "</div>", "\n").Replace(s)
	s = tagRe.ReplaceAllString(s, "")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	return spaceRe.ReplaceAllString(s, "\n\n")
}

// sendEmail replies to the sender of a conversation.
func (m *Manager) sendEmail(ctx context.Context, ch *model.Channel, to, text string) error {
	password, err := m.svc.ChannelSecret(ch.ID)
	if err != nil || password == "" {
		return fmt.Errorf("no mailbox password")
	}
	m.mu.Lock()
	ref := m.mail[ch.ID+"/"+to]
	m.mu.Unlock()
	subject := ref.Subject
	if subject == "" {
		subject = "Message from your assistant"
	} else if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	from := setting(*ch, "address", "")
	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%d.zpilot@%s>\r\n",
		from, to, mime.QEncoding.Encode("utf-8", subject), time.Now().Format(time.RFC1123Z), time.Now().UnixNano(), from[strings.LastIndex(from, "@")+1:])
	if ref.MessageID != "" {
		fmt.Fprintf(&msg, "In-Reply-To: %s\r\nReferences: %s\r\n", ref.MessageID, ref.MessageID)
	}
	msg.WriteString("Auto-Submitted: auto-replied\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(&msg)
	qp.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n")))
	qp.Close()
	return sendSMTP(ctx, *ch, password, from, to, msg.Bytes())
}

func sendSMTP(ctx context.Context, ch model.Channel, password, from, to string, body []byte) error {
	host := setting(ch, "smtp_host", "")
	mode := setting(ch, "smtp_tls", "starttls")
	port := setting(ch, "smtp_port", map[string]string{"tls": "465", "starttls": "587", "none": "25"}[mode])
	addr := net.JoinHostPort(host, port)
	d := &net.Dialer{Timeout: 30 * time.Second}
	var conn net.Conn
	var err error
	if mode == "tls" {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if mode == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	user := setting(ch, "username", from)
	// PlainAuth refuses to send credentials without TLS unless the server is local.
	if err := c.Auth(smtp.PlainAuth("", user, password, host)); err != nil {
		return fmt.Errorf("smtp login: %w", err)
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
