package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseDotenv(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, ".env", "\xef\xbb\xbf# comment\n\nPROVIDER=gemini\nexport MODEL = m1  # trailing\nAPI_KEY=\"a b\\n#c\"\nBASE_URL='https://h.example/x y'\nEMPTY=\n")
	got, err := ParseDotenv(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PROVIDER": "gemini", "MODEL": "m1", "API_KEY": "a b\n#c", "BASE_URL": "https://h.example/x y", "EMPTY": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
	for _, bad := range []string{"NOEQUALS\n", "=v\n", "A=\"unterminated\n", "A='x\n"} {
		if _, err := ParseDotenv(write(t, dir, "bad.env", bad)); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestLoadEnvValues(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, ".env", "PROVIDER=OpenAI_Compatible\nBASE_URL=https://api.zyreca.com\nAPI_KEY=k\nMODEL=m\nCONTEXT_LIMIT=1m\nCOMPACT_AT=80%\nVISION=1\nAUDIO=no\nPDF=true\n")
	e, err := LoadEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Provider != "openai_compatible" || e.ContextLimit != 1_000_000 || e.CompactAt != 800_000 {
		t.Fatalf("env: %+v", e)
	}
	if !e.Caps.Vision || e.Caps.Audio || !e.Caps.PDF || e.Caps.Video {
		t.Fatalf("caps: %+v", e.Caps)
	}
	if probs := e.Validate(); len(probs) != 0 {
		t.Fatalf("unexpected problems: %v", probs)
	}
}

func TestLoadEnvProblemsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, ".env", "PROVIDER=gemini\nAPI_KEY=file\nMODEL=m\nCOMPACT_AT=80%\nVISION=maybe\nCONTEXT_LIMIT=abc\n")
	if _, err := LoadEnv(p); err == nil || !strings.Contains(err.Error(), "VISION") || !strings.Contains(err.Error(), "CONTEXT_LIMIT") {
		t.Fatalf("want aggregated errors, got %v", err)
	}
	p = write(t, dir, ".env", "PROVIDER=gemini\nAPI_KEY=file\nMODEL=m\nCOMPACT_AT=90%\n")
	if _, err := LoadEnv(p); err == nil || !strings.Contains(err.Error(), "needs CONTEXT_LIMIT") {
		t.Fatalf("percent without limit: %v", err)
	}
	p = write(t, dir, ".env", "PROVIDER=gemini\nAPI_KEY=file\nMODEL=m\n")
	t.Setenv("ZAGENT_API_KEY", "from-env")
	e, err := LoadEnv(p)
	if err != nil || e.APIKey != "from-env" {
		t.Fatalf("env override failed: %+v %v", e, err)
	}
}

func TestEnvValidate(t *testing.T) {
	e := Env{Provider: "custom", APIFormat: "openai", BaseURL: "https://x/y", Model: "m", APIKey: "k", ContextLimit: 100, CompactAt: 200}
	if probs := e.Validate(); len(probs) != 1 || !strings.Contains(probs[0], "COMPACT_AT") {
		t.Fatalf("got %v", probs)
	}
	e = Env{Provider: "gemini"}
	if probs := strings.Join(e.Validate(), "|"); !strings.Contains(probs, "MODEL") || !strings.Contains(probs, "API_KEY") {
		t.Fatalf("got %v", probs)
	}
}

func TestConfigDefaultsAndFlags(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(filepath.Join(dir, "missing.json"))
	if err != nil || c.Agent.MaxToolCallTurn != 200 || !bool(c.Context.AutocompactOnRequestBodyLimitExceed) {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	p := write(t, dir, "config.json", `{"context":{"autocompact_on_request_body_limit_exceed":0,"remove_old_attachment_on_request_body_limit":"false","max_turn_to_keep_attachment":3},"network":{"auto_pause":1},"tools":{"disabled":["run_command"]}}`)
	c, err = LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if bool(c.Context.AutocompactOnRequestBodyLimitExceed) || bool(c.Context.RemoveOldAttachmentOnRequestBodyLimit) || c.Context.MaxTurnToKeepAttachment != 3 {
		t.Fatalf("flags: %+v", c.Context)
	}
	if !bool(c.Network.AutoPause) || len(c.Tools.Disabled) != 1 {
		t.Fatalf("cfg: %+v", c)
	}
	if c.Context.RequestBodyLimitMB != 28 { // untouched default survives a partial file
		t.Fatalf("partial file clobbered defaults: %+v", c.Context)
	}
	if probs := c.Validate(); len(probs) != 0 {
		t.Fatalf("problems: %v", probs)
	}
}

func TestConfigRejectsUnknownKeysAndBadValues(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadConfig(write(t, dir, "a.json", `{"contxt":{}}`)); err == nil {
		t.Fatal("typo'd key must be rejected")
	}
	if _, err := LoadConfig(write(t, dir, "b.json", `{"context":{"autocompact_on_request_body_limit_exceed":"maybe"}}`)); err == nil {
		t.Fatal("bad flag must be rejected")
	}
	c := Defaults()
	c.Permissions.RunCommand = "sometimes"
	c.Provider.GeminiToolMedia = "x"
	c.Context.RequestBodyLimitMB = 0
	if probs := strings.Join(c.Validate(), "|"); !strings.Contains(probs, "run_command") || !strings.Contains(probs, "gemini_tool_media") || !strings.Contains(probs, "request_body_limit_mb") {
		t.Fatalf("validate: %s", probs)
	}
}

func TestHomeInitIsIdempotentAndNonDestructive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "home")
	h := Home{Root: root}
	created, err := h.Init()
	if err != nil || len(created) < 8 {
		t.Fatalf("init: %v %v", created, err)
	}
	// the generated templates must themselves load and validate
	env, cfg, err := h.Load()
	if err != nil {
		t.Fatalf("templates do not load: %v", err)
	}
	if probs := cfg.Validate(); len(probs) != 0 {
		t.Fatalf("template config invalid: %v", probs)
	}
	if env.CompactAt != 800_000 {
		t.Fatalf("template env: %+v", env)
	}
	if err := os.WriteFile(h.EnvFile(), []byte("PROVIDER=custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := h.Init()
	if err != nil || len(again) != 0 {
		t.Fatalf("second init should create nothing: %v %v", again, err)
	}
	if b, _ := os.ReadFile(h.EnvFile()); string(b) != "PROVIDER=custom\n" {
		t.Fatal("init overwrote an existing file")
	}
}

func TestResolveHomePrecedence(t *testing.T) {
	t.Setenv("ZAGENT_HOME", "")
	h, err := ResolveHome("")
	if err != nil || h.Root == "" {
		t.Fatalf("exe fallback: %v %v", h, err)
	}
	t.Setenv("ZAGENT_HOME", t.TempDir())
	h, _ = ResolveHome("")
	if h.Root != os.Getenv("ZAGENT_HOME") {
		t.Fatalf("env: %s", h.Root)
	}
	flag := t.TempDir()
	h, _ = ResolveHome(flag)
	if h.Root != flag {
		t.Fatalf("flag must win: %s", h.Root)
	}
}

func TestAPIKeyNotRequiredForUnauthenticatedLocalServers(t *testing.T) {
	e := Env{Provider: "openai_compatible", BaseURL: "http://localhost:11434", Model: "llama3"}
	if probs := strings.Join(e.Validate(), "|"); !strings.Contains(probs, "API_KEY is required") {
		t.Fatalf("a key is required by default: %v", probs)
	}
	c := Defaults()
	c.Provider.AuthStyle = "none"
	if probs := e.ValidateFor(c); len(probs) != 0 {
		t.Fatalf("auth_style none must not demand a key: %v", probs)
	}
}
