package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
)

// WhatsApp uses Meta's official Cloud API. Settings:
//
//	phone_number_id   the sender number's id from the Meta app dashboard
//	verify_token      any string; enter the same in Meta's webhook setup
//	graph_version     default v21.0
//
// Secrets: the access token (main secret) and the app secret (second secret),
// which verifies the X-Hub-Signature-256 of every webhook delivery.
// Meta must reach https://<your host>/hooks/whatsapp/<channel id>, so this
// channel needs ZPilot reachable over HTTPS (a VPS or a tunnel).

// GraphAPI is the Meta Graph API base URL (overridable in tests).
var GraphAPI = "https://graph.facebook.com"

// WhatsAppVerify answers Meta's webhook verification handshake.
func WhatsAppVerify(ch model.Channel, mode, token, challenge string) (string, bool) {
	want := setting(ch, "verify_token", "")
	if mode != "subscribe" || want == "" || !hmac.Equal([]byte(token), []byte(want)) {
		return "", false
	}
	return challenge, true
}

// WhatsAppSignatureOK checks X-Hub-Signature-256 against the app secret.
func WhatsAppSignatureOK(appSecret string, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || appSecret == "" {
		return false
	}
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

type waWebhook struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []struct {
					From string `json:"from"`
					ID   string `json:"id"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// WhatsAppInbound processes a verified webhook delivery.
func (m *Manager) WhatsAppInbound(ctx context.Context, ch model.Channel, body []byte) error {
	var hook waWebhook
	if err := json.Unmarshal(body, &hook); err != nil {
		return err
	}
	for _, e := range hook.Entry {
		for _, c := range e.Changes {
			names := map[string]string{}
			for _, ct := range c.Value.Contacts {
				names[ct.WaID] = ct.Profile.Name
			}
			for _, msg := range c.Value.Messages {
				from := msg.From
				reply := func(t string) { _ = m.sendWhatsApp(context.WithoutCancel(ctx), &ch, from, t) }
				if msg.Type != "text" {
					reply("I can only read text messages for now.")
					continue
				}
				m.inbound(ctx, ch, core.Inbound{ChannelID: ch.ID, ChatID: from, SenderID: from, SenderName: nonEmpty(names[from], "+"+from), Text: msg.Text.Body},
					reply, "WhatsApp number")
			}
		}
	}
	return nil
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (m *Manager) sendWhatsApp(ctx context.Context, ch *model.Channel, to, text string) error {
	token, err := m.svc.ChannelSecret(ch.ID)
	if err != nil || token == "" {
		return fmt.Errorf("no access token")
	}
	url := fmt.Sprintf("%s/%s/%s/messages", GraphAPI, setting(*ch, "graph_version", "v21.0"), setting(*ch, "phone_number_id", ""))
	for _, part := range split(text, 4000) {
		b, _ := json.Marshal(map[string]any{"messaging_product": "whatsapp", "recipient_type": "individual", "to": to,
			"type": "text", "text": map[string]any{"body": part, "preview_url": false}})
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, _ := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := m.http.Do(req)
		if err != nil {
			cancel()
			return redact(err, token)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		cancel()
		if resp.StatusCode >= 300 {
			var e struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(data, &e)
			return fmt.Errorf("whatsapp send: HTTP %d %s", resp.StatusCode, e.Error.Message)
		}
	}
	return nil
}
