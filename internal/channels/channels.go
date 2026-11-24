// Package channels connects external messaging services to agents.
//
// Telegram uses long polling (no public URL needed, works behind NAT and on a
// laptop). Webhooks are served by the HTTP server (see package server); this
// package only provides their outbound side, which is the synchronous reply.
//
// Every inbound message is checked against the channel's allow-list in core;
// unknown senders are refused and shown to the owner, who can allow them.
package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
)

// TelegramAPI is the Bot API base URL (overridable in tests).
var TelegramAPI = "https://api.telegram.org"

// Manager runs one poller per enabled Telegram channel and implements
// core.Outbound.
type Manager struct {
	svc  *core.Service
	http *http.Client
	log  func(string, ...any)

	mu      sync.Mutex
	pollers map[string]*poller // channel id -> poller
	notice  map[string]time.Time
	// mail remembers the subject and message id of the last e-mail per
	// conversation so replies thread correctly.
	mail map[string]mailRef
	// discordAPI overrides the Discord REST base URL (tests).
	discordAPI string
}

type poller struct {
	cancel context.CancelFunc
	token  string // signature of the secret and settings the poller runs with
	done   chan struct{}
}

// NewManager creates the channel manager. Call Run to start it.
func NewManager(svc *core.Service, log func(string, ...any)) *Manager {
	return &Manager{svc: svc, http: &http.Client{Timeout: 70 * time.Second}, log: log,
		pollers: map[string]*poller{}, notice: map[string]time.Time{}, mail: map[string]mailRef{}, discordAPI: DiscordAPI}
}

// Run reconciles pollers with the configured channels until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	events, unsub := m.svc.Events().Subscribe()
	defer unsub()
	m.reconcile(ctx)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Type == "channels" {
				m.reconcile(ctx)
			}
		case <-tick.C:
			m.reconcile(ctx)
		}
	}
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	ps := m.pollers
	m.pollers = map[string]*poller{}
	m.mu.Unlock()
	for _, p := range ps {
		p.cancel()
		<-p.done
	}
}

// reconcile starts a connection for every enabled Telegram, Discord and
// Email channel and stops the rest. A channel restarts when its secret or
// settings change.
func (m *Manager) reconcile(ctx context.Context) {
	type target struct {
		ch  core.ChannelView
		tok string
		sig string
	}
	want := map[string]target{}
	for _, c := range m.svc.Channels() {
		if !c.Enabled || !c.HasSecret {
			continue
		}
		switch c.Kind {
		case model.ChannelTelegram, model.ChannelDiscord, model.ChannelEmail:
		default:
			continue // webhooks and WhatsApp are inbound HTTP, served by the server
		}
		tok, err := m.svc.ChannelSecret(c.ID)
		if err != nil || tok == "" {
			continue
		}
		sig, _ := json.Marshal([]any{tok, c.Settings, c.AgentID})
		want[c.ID] = target{c, tok, string(sig)}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, p := range m.pollers {
		if t, ok := want[id]; !ok || t.sig != p.token {
			p.cancel()
			delete(m.pollers, id)
			m.svc.SetChannelStatus(id, "stopped")
		}
	}
	for id, t := range want {
		if _, ok := m.pollers[id]; ok {
			continue
		}
		pctx, cancel := context.WithCancel(ctx)
		p := &poller{cancel: cancel, token: t.sig, done: make(chan struct{})}
		m.pollers[id] = p
		go func(t target) {
			defer close(p.done)
			switch t.ch.Kind {
			case model.ChannelTelegram:
				m.pollTelegram(pctx, t.ch.ID, t.tok)
			case model.ChannelDiscord:
				m.runDiscord(pctx, t.ch.ID, t.tok)
			case model.ChannelEmail:
				m.pollEmail(pctx, t.ch.ID, t.tok)
			}
		}(t)
	}
}

// ---- telegram ----------------------------------------------------------

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64 `json:"message_id"`
		From      *struct {
			ID        int64  `json:"id"`
			IsBot     bool   `json:"is_bot"`
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
			Username  string `json:"username"`
		} `json:"from"`
		Chat struct {
			ID    int64  `json:"id"`
			Type  string `json:"type"`
			Title string `json:"title"`
		} `json:"chat"`
		Text    string `json:"text"`
		Caption string `json:"caption"`
	} `json:"message"`
}

type tgResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (m *Manager) call(ctx context.Context, token, method string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TelegramAPI+"/bot"+token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return redact(err, token)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	var r tgResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d, unexpected response", method, resp.StatusCode)
	}
	if !r.OK {
		if r.Parameters != nil && r.Parameters.RetryAfter > 0 {
			return &retryAfter{time.Duration(r.Parameters.RetryAfter) * time.Second, r.Description}
		}
		return fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

type retryAfter struct {
	d   time.Duration
	msg string
}

func (e *retryAfter) Error() string { return "telegram rate limit: " + e.msg }

// redact keeps the bot token out of logged errors (it is part of the URL).
func redact(err error, token string) error {
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "<token>"))
}

func (m *Manager) pollTelegram(ctx context.Context, channelID, token string) {
	var me struct {
		Username string `json:"username"`
	}
	backoff := time.Second
	for ctx.Err() == nil {
		if err := m.call(ctx, token, "getMe", map[string]any{}, &me); err != nil {
			m.svc.SetChannelStatus(channelID, "error: "+err.Error())
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}
		break
	}
	m.svc.SetChannelStatus(channelID, "connected as @"+me.Username)
	backoff = time.Second
	for ctx.Err() == nil {
		ch, err := m.svc.Channel(channelID)
		if err != nil {
			return
		}
		var updates []tgUpdate
		err = m.call(ctx, token, "getUpdates", map[string]any{"offset": ch.Offset, "timeout": 50, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := backoff
			if ra, ok := err.(*retryAfter); ok {
				wait = ra.d
			}
			m.svc.SetChannelStatus(channelID, "error: "+err.Error())
			if !sleep(ctx, wait) {
				return
			}
			backoff = min(backoff*2, 2*time.Minute)
			continue
		}
		backoff = time.Second
		m.svc.SetChannelStatus(channelID, "connected as @"+me.Username)
		next := ch.Offset
		for _, u := range updates {
			if u.UpdateID >= next {
				next = u.UpdateID + 1
			}
			m.handleTelegram(ctx, ch.Channel, token, u)
		}
		if next != ch.Offset {
			m.svc.SetChannelOffset(channelID, next)
		}
	}
}

func (m *Manager) handleTelegram(ctx context.Context, ch model.Channel, token string, u tgUpdate) {
	msg := u.Message
	if msg == nil || msg.From == nil || msg.From.IsBot {
		return
	}
	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	senderID := strconv.FormatInt(msg.From.ID, 10)
	name := strings.TrimSpace(msg.From.FirstName + " " + msg.From.LastName)
	if msg.From.Username != "" {
		name = strings.TrimSpace(name + " @" + msg.From.Username)
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		text = strings.TrimSpace(msg.Caption)
	}
	reply := func(t string) { _ = m.Send(ctx, &ch, chatID, t) }
	if text == "" {
		reply("I can only read text messages for now.")
		return
	}
	if text == "/start" || strings.HasPrefix(text, "/start ") {
		text = "Hello! (The user just opened this chat.)"
	}
	_, allowed, err := m.svc.ChannelInbound(core.Inbound{ChannelID: ch.ID, ChatID: chatID, SenderID: senderID, SenderName: name, Text: text})
	switch {
	case err != nil:
		m.log("telegram %s: %v", ch.Name, err)
		reply("Sorry, the message could not be delivered: " + err.Error())
	case !allowed:
		key := ch.ID + "/" + senderID
		m.mu.Lock()
		last := m.notice[key]
		send := time.Since(last) > 10*time.Minute
		if send {
			m.notice[key] = time.Now()
		}
		m.mu.Unlock()
		if send {
			reply(fmt.Sprintf("This assistant is private. Ask its owner to allow your Telegram id %s in ZPilot (Channels → %s).", senderID, ch.Name))
		}
	}
}

// Send implements core.Outbound.
func (m *Manager) Send(ctx context.Context, ch *model.Channel, chatID, text string) error {
	switch ch.Kind {
	case model.ChannelDiscord:
		return m.sendDiscord(ctx, ch, chatID, text)
	case model.ChannelEmail:
		return m.sendEmail(ctx, ch, chatID, text)
	case model.ChannelWhatsApp:
		return m.sendWhatsApp(ctx, ch, chatID, text)
	case model.ChannelTelegram:
	default:
		return nil // webhook replies are returned synchronously by the HTTP server
	}
	token, err := m.svc.ChannelSecret(ch.ID)
	if err != nil || token == "" {
		return fmt.Errorf("no bot token")
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("bad chat id %q", chatID)
	}
	for _, part := range split(text, 4000) {
		for attempt := 0; ; attempt++ {
			err := m.call(ctx, token, "sendMessage", map[string]any{"chat_id": id, "text": part, "disable_web_page_preview": true}, nil)
			if ra, ok := err.(*retryAfter); ok && attempt < 3 {
				if !sleep(ctx, ra.d) {
					return ctx.Err()
				}
				continue
			}
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// Typing implements core.Outbound.
func (m *Manager) Typing(ctx context.Context, ch *model.Channel, chatID string) {
	if ch.Kind == model.ChannelDiscord {
		m.typingDiscord(ctx, ch, chatID)
		return
	}
	if ch.Kind != model.ChannelTelegram {
		return
	}
	token, err := m.svc.ChannelSecret(ch.ID)
	if err != nil || token == "" {
		return
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = m.call(cctx, token, "sendChatAction", map[string]any{"chat_id": id, "action": "typing"}, nil)
}

// split breaks text into chunks of at most n bytes, preferring line breaks
// and never cutting a UTF-8 character.
func split(text string, n int) []string {
	if text == "" {
		return []string{"(empty reply)"}
	}
	var out []string
	for len(text) > n {
		cut := strings.LastIndex(text[:n], "\n")
		if cut < n/2 {
			cut = n
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		out = append(out, strings.TrimRight(text[:cut], "\n"))
		text = strings.TrimLeft(text[cut:], "\n")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
