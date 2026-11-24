package core

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/model"
)

// ChannelView is a channel plus whether its secret is set.
type ChannelView struct {
	model.Channel
	HasSecret  bool   `json:"has_secret"`
	HasSecret2 bool   `json:"has_secret2"`
	Agent      string `json:"agent"`
}

// Channels lists channels.
func (s *Service) Channels() []ChannelView {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	out := []ChannelView{}
	for _, c := range s.st.S.Channels {
		out = append(out, s.channelViewLocked(c))
	}
	return out
}

func (s *Service) channelViewLocked(c *model.Channel) ChannelView {
	v := ChannelView{Channel: *c, HasSecret: s.vault.Has(c.SecretRef), HasSecret2: s.vault.Has(c.Secret2Ref)}
	v.Settings = maps.Clone(c.Settings)
	v.Allowed = slices.Clone(c.Allowed)
	v.Rejected = slices.Clone(c.Rejected)
	if a := s.st.Agent(c.AgentID); a != nil {
		v.Agent = a.Username
	}
	return v
}

// Channel returns one channel (copy).
func (s *Service) Channel(id string) (ChannelView, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	c := s.st.Channel(id)
	if c == nil {
		return ChannelView{}, ErrNotFound
	}
	return s.channelViewLocked(c), nil
}

// ChannelSecret returns a channel's main secret (bot token, password, ...).
func (s *Service) ChannelSecret(id string) (string, error) {
	s.st.Mu.Lock()
	c := s.st.Channel(id)
	s.st.Mu.Unlock()
	if c == nil {
		return "", ErrNotFound
	}
	return s.vault.Get(c.SecretRef)
}

// ChannelSecret2 returns a channel's second secret (whatsapp app secret).
func (s *Service) ChannelSecret2(id string) (string, error) {
	s.st.Mu.Lock()
	c := s.st.Channel(id)
	s.st.Mu.Unlock()
	if c == nil {
		return "", ErrNotFound
	}
	return s.vault.Get(c.Secret2Ref)
}

// SaveChannel creates or updates a channel. A nil secret keeps the stored one.
func (s *Service) SaveChannel(actor string, in model.Channel, secret *string) (ChannelView, error) {
	return s.SaveChannel2(actor, in, secret, nil)
}

// SaveChannel2 is SaveChannel with a second secret (nil keeps the stored one).
func (s *Service) SaveChannel2(actor string, in model.Channel, secret, secret2 *string) (ChannelView, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	if a := s.st.AgentRef(in.AgentID); a != nil {
		in.AgentID = a.ID
	}
	if err := in.Validate(); err != nil {
		return ChannelView{}, bad("%v", err)
	}
	a := s.st.Agent(in.AgentID)
	if a == nil {
		return ChannelView{}, bad("agent does not exist")
	}
	if in.Enabled && a.Level.RunsCommands() && !a.ExternalInputWithCommands && !a.ApproveCommands {
		return ChannelView{}, bad("@%s can run commands (level %s). Messages from a channel are untrusted, so lower its level, require approval for every command, or confirm 'external input with commands' on the agent first", a.Username, a.Level)
	}
	now := time.Now().UTC()
	var cur *model.Channel
	if in.ID != "" {
		if cur = s.st.Channel(in.ID); cur == nil {
			return ChannelView{}, ErrNotFound
		}
		if cur.Kind != in.Kind {
			return ChannelView{}, bad("a channel's kind cannot change")
		}
		in.Created, in.SecretRef, in.Secret2Ref, in.Offset, in.Rejected = cur.Created, cur.SecretRef, cur.Secret2Ref, cur.Offset, cur.Rejected
	} else {
		in.ID = model.NewID("chn")
		in.Created = now
		in.SecretRef = "channel/" + in.ID
	}
	if in.Secret2Ref == "" {
		in.Secret2Ref = "channel2/" + in.ID
	}
	in.Updated = now
	in.Allowed = cleanList(in.Allowed)
	if secret != nil {
		if err := s.vault.Set(in.SecretRef, strings.TrimSpace(*secret)); err != nil {
			return ChannelView{}, err
		}
	}
	if secret2 != nil {
		if err := s.vault.Set(in.Secret2Ref, strings.TrimSpace(*secret2)); err != nil {
			return ChannelView{}, err
		}
	}
	if in.Enabled && !s.vault.Has(in.SecretRef) {
		return ChannelView{}, bad("set the %s first", map[string]string{model.ChannelTelegram: "bot token", model.ChannelDiscord: "bot token",
			model.ChannelWebhook: "shared secret", model.ChannelEmail: "mailbox password", model.ChannelWhatsApp: "access token"}[in.Kind])
	}
	if in.Enabled && in.Kind == model.ChannelWhatsApp && !s.vault.Has(in.Secret2Ref) {
		return ChannelView{}, bad("set the WhatsApp app secret first (used to verify that webhooks come from Meta)")
	}
	if cur != nil {
		*cur = in
	} else {
		c := in
		cur = &c
		s.st.S.Channels = append(s.st.S.Channels, cur)
	}
	if err := s.st.Save(); err != nil {
		return ChannelView{}, err
	}
	s.st.Audit(actor, "channel.save", in.Name, in.Kind+" -> @"+a.Username)
	s.hub.Publish("channels", nil)
	return s.channelViewLocked(cur), nil
}

// DeleteChannel removes a channel and its secret.
func (s *Service) DeleteChannel(actor, id string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	c := s.st.Channel(id)
	if c == nil {
		return ErrNotFound
	}
	s.st.S.Channels = slices.DeleteFunc(s.st.S.Channels, func(x *model.Channel) bool { return x.ID == id })
	_ = s.vault.Delete(c.SecretRef)
	_ = s.vault.Delete(c.Secret2Ref)
	for k := range s.st.S.ChannelThreads {
		if strings.HasPrefix(k, id+"/") {
			delete(s.st.S.ChannelThreads, k)
		}
	}
	if err := s.st.Save(); err != nil {
		return err
	}
	s.st.Audit(actor, "channel.delete", c.Name, "")
	s.hub.Publish("channels", nil)
	return nil
}

// SetChannelStatus records a runtime status line (no audit, no persistence churn).
func (s *Service) SetChannelStatus(id, status string) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	if c := s.st.Channel(id); c != nil && c.Status != status {
		c.Status = status
		s.hub.Publish("channels", nil)
	}
}

// SetChannelOffset persists a Telegram update offset.
func (s *Service) SetChannelOffset(id string, off int64) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	if c := s.st.Channel(id); c != nil && c.Offset != off {
		c.Offset = off
		_ = s.st.Save()
	}
}

// Inbound is a message arriving from a channel.
type Inbound struct {
	ChannelID  string
	ChatID     string // external conversation id
	SenderID   string // external user id (checked against Allowed)
	SenderName string
	Text       string
}

// ChannelInbound routes an external message to the channel's agent. Unknown
// senders are refused and remembered so the owner can allow them.
func (s *Service) ChannelInbound(in Inbound) (*model.Message, bool, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	c := s.st.Channel(in.ChannelID)
	if c == nil || !c.Enabled {
		return nil, false, ErrNotFound
	}
	// Webhook callers proved themselves with the channel secret; chat services
	// are checked against the allow-list.
	allowed := c.Kind == model.ChannelWebhook ||
		slices.Contains(c.Allowed, in.SenderID) || slices.Contains(c.Allowed, in.ChatID) || slices.Contains(c.Allowed, "*") ||
		// e-mail: an entry "@example.com" allows the whole domain
		strings.Contains(in.SenderID, "@") && slices.Contains(c.Allowed, in.SenderID[strings.LastIndex(in.SenderID, "@"):])
	if !allowed {
		now := time.Now().UTC()
		i := slices.IndexFunc(c.Rejected, func(r model.Sender) bool { return r.ID == in.SenderID })
		if i < 0 {
			c.Rejected = append(c.Rejected, model.Sender{ID: in.SenderID, Name: in.SenderName, Last: now, Text: clip(oneLine(in.Text), 80)})
			if len(c.Rejected) > 20 {
				c.Rejected = c.Rejected[len(c.Rejected)-20:]
			}
			s.st.Audit("channel:"+c.Name, "channel.reject", in.SenderID, in.SenderName)
		} else {
			c.Rejected[i].Last, c.Rejected[i].Text = now, clip(oneLine(in.Text), 80)
		}
		_ = s.st.Save()
		s.hub.Publish("channels", nil)
		return nil, false, nil
	}
	a := s.st.Agent(c.AgentID)
	if a == nil {
		return nil, true, bad("channel agent missing")
	}
	key := c.ID + "/" + in.ChatID
	threadID := s.st.S.ChannelThreads[key]
	if threadID != "" && s.st.Thread(threadID) == nil {
		threadID = ""
	}
	m, err := s.sendLocked(SendInput{ThreadID: threadID, To: a.Username, Text: in.Text,
		From: "channel:" + c.ID + ":" + in.ChatID, FromName: in.SenderName + " (" + c.Kind + ")", Origin: "channel:" + c.ID})
	if err != nil {
		return nil, true, err
	}
	if threadID == "" {
		s.st.S.ChannelThreads[key] = m.ThreadID
		if t := s.st.Thread(m.ThreadID); t != nil {
			t.Title = c.Kind + " · " + nonEmpty(in.SenderName, in.ChatID)
			t.ChannelRef = key
		}
		_ = s.st.Save()
	}
	return m, true, nil
}

// AllowSender adds an external id to a channel's allow-list.
func (s *Service) AllowSender(actor, channelID, senderID string, allow bool) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	c := s.st.Channel(channelID)
	if c == nil {
		return ErrNotFound
	}
	senderID = strings.TrimSpace(senderID)
	if senderID == "" {
		return bad("sender id is required")
	}
	if allow {
		if !slices.Contains(c.Allowed, senderID) {
			c.Allowed = append(c.Allowed, senderID)
		}
	} else {
		c.Allowed = slices.DeleteFunc(c.Allowed, func(x string) bool { return x == senderID })
	}
	c.Rejected = slices.DeleteFunc(c.Rejected, func(r model.Sender) bool { return r.ID == senderID })
	if err := s.st.Save(); err != nil {
		return err
	}
	verb := map[bool]string{true: "channel.allow", false: "channel.disallow"}[allow]
	s.st.Audit(actor, verb, c.Name, senderID)
	s.hub.Publish("channels", nil)
	return nil
}

// ChannelTyping tells the channel a reply is being prepared (best effort).
func (s *Service) channelForAddr(addr string) (*model.Channel, string) {
	rest, ok := strings.CutPrefix(addr, "channel:")
	if !ok {
		return nil, ""
	}
	id, chat, _ := strings.Cut(rest, ":")
	c := s.st.Channel(id)
	if c == nil {
		return nil, ""
	}
	cc := *c
	return &cc, chat
}

func cleanList(in []string) []string {
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
