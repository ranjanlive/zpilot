package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/ws"
)

// DiscordAPI is the REST base URL (overridable in tests).
var DiscordAPI = "https://discord.com/api/v10"

// Gateway intents: guild messages, direct messages, message content.
const discordIntents = 1<<9 | 1<<12 | 1<<15

type gwPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

type discordMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Content   string `json:"content"`
	Author    struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
		Bot        bool   `json:"bot"`
	} `json:"author"`
	Mentions []struct {
		ID string `json:"id"`
	} `json:"mentions"`
}

// discordREST performs a Discord REST call with the bot token.
func (m *Manager) discordREST(ctx context.Context, token, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, m.discordAPI+path, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "DiscordBot (https://github.com/ranjanlive/zpilot, 0.1)")
		resp, err := m.http.Do(req)
		if err != nil {
			return redact(err, token)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			_ = json.Unmarshal(data, &rl)
			if !sleep(ctx, time.Duration(max(rl.RetryAfter, 0.5)*float64(time.Second))) {
				return ctx.Err()
			}
			if body != nil {
				b, _ := json.Marshal(body)
				rd = bytes.NewReader(b)
			}
			continue
		}
		if resp.StatusCode >= 300 {
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(data, &e)
			return fmt.Errorf("discord %s %s: HTTP %d %s", method, path, resp.StatusCode, e.Message)
		}
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

// runDiscord keeps a gateway connection open, reconnecting with backoff.
func (m *Manager) runDiscord(ctx context.Context, channelID, token string) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := m.discordSession(ctx, channelID, token)
		if ctx.Err() != nil {
			return
		}
		status := "reconnecting"
		if err != nil {
			status = "error: " + err.Error()
			var ce *ws.CloseError
			if asCloseErr(err, &ce) && (ce.Code == 4004 || ce.Code == 4013 || ce.Code == 4014) {
				// invalid token or intents: retrying will not help until the owner fixes it
				m.svc.SetChannelStatus(channelID, "error: "+discordCloseReason(ce.Code))
				if !sleep(ctx, 10*time.Minute) {
					return
				}
				continue
			}
		}
		m.svc.SetChannelStatus(channelID, status)
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func asCloseErr(err error, ce **ws.CloseError) bool {
	c, ok := err.(*ws.CloseError)
	*ce = c
	return ok
}

func discordCloseReason(code int) string {
	switch code {
	case 4004:
		return "the bot token is invalid"
	case 4013, 4014:
		return "enable the MESSAGE CONTENT intent for the bot in the Discord developer portal"
	}
	return fmt.Sprintf("gateway closed with code %d", code)
}

func (m *Manager) discordSession(ctx context.Context, channelID, token string) error {
	var gw struct {
		URL string `json:"url"`
	}
	if err := m.discordREST(ctx, token, "GET", "/gateway/bot", nil, &gw); err != nil {
		return err
	}
	u, err := url.Parse(gw.URL)
	if err != nil {
		return err
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawQuery = "v=10&encoding=json"
	conn, err := ws.Dial(u.String(), nil, 20*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close(1000)
	stop := context.AfterFunc(ctx, func() { conn.Close(1000) })
	defer stop()

	var (
		seq   *int64
		botID string
	)
	heartbeat := func() error {
		b, _ := json.Marshal(map[string]any{"op": 1, "d": seq})
		return conn.WriteText(b)
	}
	hbDone := make(chan struct{})
	defer close(hbDone)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var p gwPayload
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}
		if p.S != nil {
			s := *p.S
			seq = &s
		}
		switch p.Op {
		case 10: // hello
			var h struct {
				HeartbeatInterval int `json:"heartbeat_interval"`
			}
			_ = json.Unmarshal(p.D, &h)
			go func(interval time.Duration) {
				t := time.NewTicker(interval)
				defer t.Stop()
				for {
					select {
					case <-hbDone:
						return
					case <-t.C:
						if heartbeat() != nil {
							return
						}
					}
				}
			}(time.Duration(max(h.HeartbeatInterval, 1000)) * time.Millisecond)
			ident, _ := json.Marshal(map[string]any{"op": 2, "d": map[string]any{
				"token": token, "intents": discordIntents,
				"properties": map[string]string{"os": runtime.GOOS, "browser": "zpilot", "device": "zpilot"},
			}})
			if err := conn.WriteText(ident); err != nil {
				return err
			}
		case 1: // server asks for a heartbeat now
			_ = heartbeat()
		case 7, 9: // reconnect / invalid session
			return nil
		case 0:
			switch p.T {
			case "READY":
				var r struct {
					User struct {
						ID       string `json:"id"`
						Username string `json:"username"`
					} `json:"user"`
				}
				_ = json.Unmarshal(p.D, &r)
				botID = r.User.ID
				m.svc.SetChannelStatus(channelID, "connected as "+r.User.Username)
			case "MESSAGE_CREATE":
				var msg discordMessage
				if json.Unmarshal(p.D, &msg) == nil {
					go m.handleDiscord(ctx, channelID, token, botID, msg)
				}
			}
		}
	}
}

func (m *Manager) handleDiscord(ctx context.Context, channelID, token, botID string, msg discordMessage) {
	if msg.Author.Bot || msg.Author.ID == botID {
		return
	}
	text := strings.TrimSpace(msg.Content)
	if msg.GuildID != "" {
		// In servers the bot answers only when mentioned.
		mentioned := false
		for _, u := range msg.Mentions {
			mentioned = mentioned || u.ID == botID
		}
		if !mentioned {
			return
		}
		text = strings.TrimSpace(strings.NewReplacer("<@"+botID+">", "", "<@!"+botID+">", "").Replace(text))
	}
	if text == "" {
		return
	}
	ch, err := m.svc.Channel(channelID)
	if err != nil {
		return
	}
	name := msg.Author.GlobalName
	if name == "" {
		name = msg.Author.Username
	}
	m.inbound(ctx, ch.Channel, core.Inbound{ChannelID: channelID, ChatID: msg.ChannelID, SenderID: msg.Author.ID, SenderName: name, Text: text},
		func(t string) { _ = m.sendDiscord(ctx, &ch.Channel, msg.ChannelID, t) }, "Discord user id")
}

func (m *Manager) sendDiscord(ctx context.Context, ch *model.Channel, chatID, text string) error {
	token, err := m.svc.ChannelSecret(ch.ID)
	if err != nil || token == "" {
		return fmt.Errorf("no bot token")
	}
	for _, part := range split(text, 1900) {
		if err := m.discordREST(ctx, token, "POST", "/channels/"+chatID+"/messages",
			map[string]any{"content": part, "allowed_mentions": map[string]any{"parse": []string{}}}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) typingDiscord(ctx context.Context, ch *model.Channel, chatID string) {
	token, err := m.svc.ChannelSecret(ch.ID)
	if err != nil || token == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = m.discordREST(cctx, token, "POST", "/channels/"+chatID+"/typing", nil, nil)
}

// inbound delivers a message and, for refused senders, tells them once in a
// while how to get access. idLabel names the id the owner must allow.
func (m *Manager) inbound(ctx context.Context, ch model.Channel, in core.Inbound, reply func(string), idLabel string) {
	_, allowed, err := m.svc.ChannelInbound(in)
	switch {
	case err != nil:
		m.log("%s %s: %v", ch.Kind, ch.Name, err)
		reply("Sorry, the message could not be delivered: " + err.Error())
	case !allowed:
		key := ch.ID + "/" + in.SenderID
		m.mu.Lock()
		send := time.Since(m.notice[key]) > 10*time.Minute
		if send {
			m.notice[key] = time.Now()
		}
		m.mu.Unlock()
		if send {
			reply(fmt.Sprintf("This assistant is private. Ask its owner to allow your %s %s in ZPilot (Channels → %s).", idLabel, in.SenderID, ch.Name))
		}
	}
}
