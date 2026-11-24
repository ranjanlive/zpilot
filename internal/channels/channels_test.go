package channels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
)

type echoEngine struct{}

func (echoEngine) Run(_ context.Context, spec core.RunSpec) core.RunResult {
	_, text, _ := strings.Cut(spec.Prompt, "]\n\n")
	return core.RunResult{Answer: "echo: " + text}
}

// fakeTelegram serves getMe/getUpdates/sendMessage like the Bot API.
type fakeTelegram struct {
	mu      sync.Mutex
	updates []map[string]any
	sent    []map[string]any
}

func (f *fakeTelegram) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(body, &req)
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.mu.Lock()
	defer f.mu.Unlock()
	res := any(true)
	switch method {
	case "getMe":
		res = map[string]any{"username": "test_bot"}
	case "getUpdates":
		off := int64(req["offset"].(float64))
		var out []map[string]any
		for _, u := range f.updates {
			if int64(u["update_id"].(int)) >= off {
				out = append(out, u)
			}
		}
		if len(out) == 0 {
			f.mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			f.mu.Lock()
		}
		res = out
	case "sendMessage":
		f.sent = append(f.sent, req)
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": res})
}

func update(id int, from int64, text string) map[string]any {
	return map[string]any{"update_id": id, "message": map[string]any{"message_id": id, "text": text,
		"from": map[string]any{"id": from, "first_name": "User"}, "chat": map[string]any{"id": from, "type": "private"}}}
}

func TestTelegramRoundTrip(t *testing.T) {
	ft := &fakeTelegram{updates: []map[string]any{update(1, 42, "hello bot"), update(2, 666, "let me in")}}
	srv := httptest.NewServer(http.HandlerFunc(ft.handler))
	defer srv.Close()
	TelegramAPI = srv.URL

	svc, err := core.Open(core.Config{DataDir: t.TempDir(), Exe: "x", Engine: echoEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.SetDaemonURL("http://127.0.0.1:1")
	key := "k"
	svc.SaveModel("t", model.ModelProfile{Name: "m", Provider: "gemini", Model: "x"}, &key)
	a, _ := svc.CreateAgent("t", model.Agent{Username: "helper", Name: "Helper"})
	tok := "123:token"
	ch, err := svc.SaveChannel("t", model.Channel{Kind: model.ChannelTelegram, Name: "tg", AgentID: a.ID, Enabled: true, Allowed: []string{"42"}}, &tok)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(svc, t.Logf)
	svc.SetOutbound(m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	svc.Start()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ft.mu.Lock()
		var toOwner, toStranger string
		for _, s := range ft.sent {
			switch s["chat_id"].(float64) {
			case 42:
				toOwner = s["text"].(string)
			case 666:
				toStranger = s["text"].(string)
			}
		}
		ft.mu.Unlock()
		if toOwner != "" && toStranger != "" {
			if toOwner != "echo: hello bot" {
				t.Fatalf("reply to allowed user: %q", toOwner)
			}
			if !strings.Contains(toStranger, "private") || strings.Contains(toStranger, "echo") {
				t.Fatalf("stranger got: %q", toStranger)
			}
			v, _ := svc.Channel(ch.ID)
			if len(v.Rejected) != 1 || v.Rejected[0].ID != "666" || v.Offset != 3 {
				t.Fatalf("channel state: rejected=%+v offset=%d", v.Rejected, v.Offset)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no replies; sent=%v", ft.sent)
}

func TestSplit(t *testing.T) {
	long := strings.Repeat("ab\n", 3000) + strings.Repeat("é", 3000)
	parts := split(long, 4000)
	joined := 0
	for _, p := range parts {
		if len(p) > 4000 || !utf8.ValidString(p) {
			t.Fatalf("bad chunk (%d bytes, valid=%v)", len(p), utf8.ValidString(p))
		}
		joined += len(p)
	}
	if len(parts) < 3 || joined < len(long)-len(parts)*2 {
		t.Fatalf("lost text: %d parts, %d of %d bytes", len(parts), joined, len(long))
	}
}
