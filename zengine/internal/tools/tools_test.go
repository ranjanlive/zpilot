package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

type testEnv struct {
	*Env
	blobs map[string][]byte
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	home := config.Home{Root: filepath.Join(root, "home")}
	for _, d := range []string{ws, home.Root, home.ScratchDir(), home.SkillsDir(), home.MemoryDir(), home.SessionsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(home.Root, ".env"), []byte("API_KEY=secret"), 0o600)
	te := &testEnv{blobs: map[string][]byte{}}
	te.Env = &Env{
		Workspace: ws, Home: home, SessionID: "s1", SessionDir: filepath.Join(home.SessionsDir(), "s1"),
		Cfg:  config.Defaults(),
		Caps: llm.Capabilities{Vision: true, PDF: true},
		PutBlob: func(b []byte) (string, error) {
			id := strings.Repeat("a", 60) + string(rune('a'+len(te.blobs)%26)) + "bcd"
			te.blobs[id] = b
			return id, nil
		},
	}
	return te
}

func call(name string, args any) llm.ToolCall {
	b, _ := json.Marshal(args)
	return llm.ToolCall{ID: "c1", Name: name, Args: b}
}

func (te *testEnv) write(t *testing.T, rel, content string) string {
	t.Helper()
	p := filepath.Join(te.Workspace, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func fileRegistry(te *testEnv) *Registry {
	r := NewRegistry(te.Cfg.Tools)
	RegisterFileTools(r)
	return r
}

func TestRegistryUnknownToolAndBadArgsAreModelReadable(t *testing.T) {
	te := newEnv(t)
	r := fileRegistry(te)
	ctx := context.Background()

	res := r.Invoke(ctx, te.Env, call("nope", map[string]any{}))
	if !res.IsError || !strings.Contains(res.Output, `unknown tool "nope"`) || !strings.Contains(res.Output, "view_file") {
		t.Fatalf("unknown tool: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"pth": "x"}))
	if !res.IsError || !strings.Contains(res.Output, `missing required argument "path"`) || !strings.Contains(res.Output, `unknown argument "pth"`) {
		t.Fatalf("bad args: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": 5}))
	if !res.IsError || !strings.Contains(res.Output, "expected string") {
		t.Fatalf("wrong type: %+v", res)
	}
}

func TestRegistryDisableAndAllowList(t *testing.T) {
	te := newEnv(t)
	te.Cfg.Tools.Disabled = []string{"write_file", "edit_file"}
	r := NewRegistry(te.Cfg.Tools)
	RegisterFileTools(r)
	for _, n := range r.Names() {
		if n == "write_file" || n == "edit_file" {
			t.Fatalf("%s should be disabled: %v", n, r.Names())
		}
	}
	res := r.Invoke(context.Background(), te.Env, call("write_file", map[string]any{"path": "a", "content": "b"}))
	if !res.IsError || !strings.Contains(res.Output, "disabled by configuration") {
		t.Fatalf("disabled call: %+v", res)
	}
	te.Cfg.Tools = config.ToolsConfig{Enabled: []string{"view_file"}, DefaultTimeoutSeconds: 5}
	r = NewRegistry(te.Cfg.Tools)
	RegisterFileTools(r)
	if got := strings.Join(r.Names(), ","); got != "view_file" {
		t.Fatalf("allow-list: %s", got)
	}
	specs := fileRegistry(newEnv(t)).Specs()
	for i := 1; i < len(specs); i++ {
		if specs[i-1].Name > specs[i].Name {
			t.Fatal("Specs must be sorted: tool order is part of the cached prefix")
		}
	}
}

type slowTool struct{ d time.Duration }

func (slowTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "slow", Description: "d", Schema: schema(map[string]any{})}
}
func (s slowTool) Run(ctx context.Context, _ *Env, _ json.RawMessage) Result {
	select {
	case <-time.After(s.d):
		return OK("done")
	case <-ctx.Done():
		return Fail("ctx")
	}
}

type panicTool struct{}

func (panicTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "boom", Description: "d", Schema: schema(map[string]any{})}
}
func (panicTool) Run(context.Context, *Env, json.RawMessage) Result { panic("kaput") }

type bigTool struct{}

func (bigTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "big", Description: "d", Schema: schema(map[string]any{})}
}
func (bigTool) Run(context.Context, *Env, json.RawMessage) Result {
	return OK("%s", strings.Repeat("é", 10000)) // multi-byte: cuts must stay on rune boundaries
}

func TestRegistryTimeoutPanicAndTruncation(t *testing.T) {
	te := newEnv(t)
	te.Cfg.Tools.DefaultTimeoutSeconds = 1
	te.Cfg.Tools.MaxOutputBytes = 1000
	r := NewRegistry(te.Cfg.Tools)
	r.Register(slowTool{d: 5 * time.Second})
	r.Register(panicTool{})
	r.Register(bigTool{})

	start := time.Now()
	res := r.Invoke(context.Background(), te.Env, call("slow", map[string]any{}))
	if !res.IsError || !strings.Contains(res.Output, "timed out") || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout: %+v after %s", res, time.Since(start))
	}
	if res := r.Invoke(context.Background(), te.Env, call("boom", map[string]any{})); !res.IsError || !strings.Contains(res.Output, "crashed: kaput") {
		t.Fatalf("panic must become an error result: %+v", res)
	}
	res = r.Invoke(context.Background(), te.Env, call("big", map[string]any{}))
	if len(res.Output) > 1200 || !strings.Contains(res.Output, "bytes omitted") {
		t.Fatalf("truncation: len=%d", len(res.Output))
	}
	if !strings.HasPrefix(res.Output, "é") || strings.ContainsRune(res.Output, '�') {
		t.Fatal("truncation split a UTF-8 rune")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if res := r.Invoke(ctx, te.Env, call("slow", map[string]any{})); !strings.Contains(res.Output, "cancelled") {
		t.Fatalf("cancel: %+v", res)
	}
}

func TestResolvePathPolicy(t *testing.T) {
	te := newEnv(t)
	inside := te.write(t, "src/a.txt", "x")
	if p, err := te.ResolvePath("src/a.txt", false); err != nil || p != inside {
		t.Fatalf("relative: %q %v", p, err)
	}
	if _, err := te.ResolvePath(inside, true); err != nil {
		t.Fatalf("absolute inside: %v", err)
	}
	if _, err := te.ResolvePath(filepath.Join(te.Home.ScratchDir(), "tmp.txt"), true); err != nil {
		t.Fatalf("scratchpad must be writable: %v", err)
	}
	if _, err := te.ResolvePath(filepath.Join(te.Home.SkillsDir(), "x"), false); err != nil {
		t.Fatalf("skills must be readable: %v", err)
	}
	if _, err := te.ResolvePath(filepath.Join(te.Home.SkillsDir(), "x"), true); err == nil {
		t.Fatal("skills must not be writable through file tools")
	}
	outside := filepath.Join(filepath.Dir(te.Workspace), "elsewhere.txt")
	for _, write := range []bool{false, true} {
		if _, err := te.ResolvePath(outside, write); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
			t.Fatalf("outside (write=%v): %v", write, err)
		}
	}
	if _, err := te.ResolvePath("../elsewhere.txt", false); err == nil {
		t.Fatal(".. escape must be refused")
	}
	if _, err := te.ResolvePath("", false); err == nil {
		t.Fatal("empty path")
	}
	// credentials are never reachable, even with every outside flag enabled
	te.Cfg.Permissions.ReadOutsideWorkspace, te.Cfg.Permissions.WriteOutsideWorkspace = true, true
	if _, err := te.ResolvePath(filepath.Join(te.Home.Root, ".env"), false); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf(".env must stay protected: %v", err)
	}
	if _, err := te.ResolvePath(filepath.Join(te.Home.SessionsDir(), "s1", "events.jsonl"), false); err == nil {
		t.Fatal("session logs must stay protected")
	}
	if _, err := te.ResolvePath(outside, true); err != nil {
		t.Fatalf("flag should open outside paths: %v", err)
	}
}

func TestResolvePathSymlinkEscape(t *testing.T) {
	te := newEnv(t)
	target := filepath.Join(filepath.Dir(te.Workspace), "secret_dir")
	os.MkdirAll(target, 0o755)
	link := filepath.Join(te.Workspace, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, err := te.ResolvePath("link/new.txt", true); err == nil {
		t.Fatal("a symlink must not lead out of the workspace")
	}
}

func TestViewFileText(t *testing.T) {
	te := newEnv(t)
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		sb.WriteString("line " + string(rune('A'+i%26)) + "\n")
	}
	te.write(t, "big.txt", sb.String())
	r := fileRegistry(te)
	ctx := context.Background()

	res := r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "big.txt", "start_line": 10, "end_line": 12}))
	if res.IsError || !strings.Contains(res.Output, "Showing lines 10-12") || !strings.Contains(res.Output, "    10\t") || strings.Contains(res.Output, "    13\t") {
		t.Fatalf("range: %+v", res)
	}
	if !strings.Contains(res.Output, "call again with start_line=13") {
		t.Fatalf("missing continuation hint: %s", res.Output)
	}
	res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "big.txt"}))
	if !strings.Contains(res.Output, "end of file") {
		t.Fatalf("full read: %s", res.Output)
	}
	res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "big.txt", "start_line": 99}))
	if !res.IsError || !strings.Contains(res.Output, "past the end") {
		t.Fatalf("past end: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "big.txt", "start_line": 5, "end_line": 2}))
	if !res.IsError {
		t.Fatalf("inverted range: %+v", res)
	}
	te.write(t, "empty.txt", "")
	if res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "empty.txt"})); !strings.Contains(res.Output, "empty") || res.IsError {
		t.Fatalf("empty: %+v", res)
	}
	if res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "missing.txt"})); !res.IsError || !strings.Contains(res.Output, "not found") {
		t.Fatalf("missing: %+v", res)
	}
	if res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "."})); !res.IsError || !strings.Contains(res.Output, "directory") {
		t.Fatalf("dir: %+v", res)
	}
	te.write(t, "long.txt", strings.Repeat("x", 9000)+"\n")
	res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "long.txt"}))
	if !strings.Contains(res.Output, "line truncated") {
		t.Fatalf("long line must be cut: %.200s", res.Output)
	}
	te.write(t, "crlf.txt", "a\r\nb\r\n")
	if res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "crlf.txt"})); strings.Contains(res.Output, "\r") {
		t.Fatal("CR must not leak into output")
	}
	os.WriteFile(filepath.Join(te.Workspace, "blob.bin"), []byte{0, 1, 2, 3, 0, 0}, 0o644)
	if res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "blob.bin"})); !res.IsError || !strings.Contains(res.Output, "binary") {
		t.Fatalf("binary: %+v", res)
	}
}

func TestViewFileMediaCapabilities(t *testing.T) {
	te := newEnv(t)
	te.Caps = llm.Capabilities{Vision: true} // PDF/audio/video off
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(te.Workspace, "pic.png"), pngData.Bytes(), 0o644)
	os.WriteFile(filepath.Join(te.Workspace, "doc.pdf"), []byte("%PDF-fake"), 0o644)
	os.WriteFile(filepath.Join(te.Workspace, "clip.mp4"), []byte("fake"), 0o644)
	r := fileRegistry(te)
	ctx := context.Background()

	res := r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "pic.png"}))
	if res.IsError || len(res.Media) != 1 || res.Media[0].Kind != llm.MediaImage || res.Media[0].MIME != "image/png" || res.Media[0].Name != "pic.png" || res.Media[0].Path == "" {
		t.Fatalf("image: %+v", res)
	}
	if len(te.blobs) != 1 {
		t.Fatal("payload not stored as a blob")
	}
	if res.Media[0].Size != int64(pngData.Len()) || !bytes.Equal(te.blobs[res.Media[0].Blob], pngData.Bytes()) {
		t.Fatalf("attachment payload is wrong: size=%d data=%q", res.Media[0].Size, te.blobs[res.Media[0].Blob])
	}
	for _, name := range []string{"doc.pdf", "clip.mp4"} {
		res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": name}))
		if !res.IsError || !strings.Contains(res.Output, "not supported by the configured model") || len(res.Media) != 0 {
			t.Fatalf("%s must be refused with a capability message: %+v", name, res)
		}
	}
	// size caps
	te.Cfg.Tools.MaxAttachMB = 0.000001 // ~1 byte
	if res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "pic.png"})); !res.IsError || !strings.Contains(res.Output, "attachment limit") {
		t.Fatalf("attachment cap: %+v", res)
	}
	te.Cfg.Tools.MaxAttachMB = 20
	te.Cfg.Context.RequestBodyLimitMB = 0.00001
	if res = r.Invoke(ctx, te.Env, call("view_file", map[string]any{"path": "pic.png"})); !res.IsError || !strings.Contains(res.Output, "request limit") {
		t.Fatalf("body-limit guard: %+v", res)
	}
}

func TestCorruptImageRejectedBeforeBlobStorage(t *testing.T) {
	te := newEnv(t)
	te.Caps = llm.Capabilities{Vision: true}
	for _, data := range [][]byte{[]byte("not a PNG"), {0xff, 0xfe, 'P', 0, 'N', 0, 'G', 0}} {
		path := filepath.Join(te.Workspace, "screen.png")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		res := fileRegistry(te).Invoke(context.Background(), te.Env, call("view_file", map[string]any{"path": "screen.png"}))
		if !res.IsError || len(res.Media) != 0 || len(te.blobs) != 0 {
			t.Fatalf("corrupt image attached: %+v", res)
		}
		if _, err := AttachFile(te.Env, path); err == nil {
			t.Fatal("prompt attachment accepted corrupt image")
		}
		if _, err := mediaFromFile(te.Env, path, "image/png"); err == nil {
			t.Fatal("external tool attachment accepted corrupt image")
		}
	}
}

func TestWriteAndEditFile(t *testing.T) {
	te := newEnv(t)
	r := fileRegistry(te)
	ctx := context.Background()

	res := r.Invoke(ctx, te.Env, call("write_file", map[string]any{"path": "new/dir/f.txt", "content": "hello\nworld\n"}))
	if res.IsError || !strings.Contains(res.Output, "Wrote 12 bytes") {
		t.Fatalf("write: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("write_file", map[string]any{"path": "new/dir/f.txt", "content": "more\n", "append": true}))
	if !strings.Contains(res.Output, "Appended") {
		t.Fatalf("append: %+v", res)
	}
	b, _ := os.ReadFile(filepath.Join(te.Workspace, "new/dir/f.txt"))
	if string(b) != "hello\nworld\nmore\n" {
		t.Fatalf("content: %q", b)
	}
	res = r.Invoke(ctx, te.Env, call("write_file", map[string]any{"path": "new/dir/f.txt", "content": "x"}))
	if !strings.Contains(res.Output, "replaced the existing file") {
		t.Fatalf("overwrite note: %+v", res)
	}
	if res = r.Invoke(ctx, te.Env, call("write_file", map[string]any{"path": "../escape.txt", "content": "x"})); !res.IsError {
		t.Fatalf("escape: %+v", res)
	}

	te.write(t, "e.txt", "alpha\nbeta\nalpha\n")
	res = r.Invoke(ctx, te.Env, call("edit_file", map[string]any{"path": "e.txt", "old_string": "alpha", "new_string": "ALPHA"}))
	if !res.IsError || !strings.Contains(res.Output, "matches 2 places") {
		t.Fatalf("ambiguous edit: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("edit_file", map[string]any{"path": "e.txt", "old_string": "alpha", "new_string": "ALPHA", "replace_all": true}))
	if res.IsError || !strings.Contains(res.Output, "replaced 2") {
		t.Fatalf("replace_all: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("edit_file", map[string]any{"path": "e.txt", "old_string": "beta", "new_string": "BETA"}))
	if res.IsError || !strings.Contains(res.Output, "replaced 1") {
		t.Fatalf("unique edit: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("edit_file", map[string]any{"path": "e.txt", "old_string": "  ALPHA", "new_string": "x"}))
	if !res.IsError || !strings.Contains(res.Output, "not found") {
		t.Fatalf("missing text: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("edit_file", map[string]any{"path": "e.txt", "old_string": "ALPHA\nnope", "new_string": "x"}))
	if !strings.Contains(res.Output, "first line of old_string does appear at line 1") {
		t.Fatalf("near-miss hint: %+v", res)
	}
	for _, bad := range []map[string]any{
		{"path": "e.txt", "old_string": "", "new_string": "x"},
		{"path": "e.txt", "old_string": "same", "new_string": "same"},
		{"path": "absent.txt", "old_string": "a", "new_string": "b"},
	} {
		if res = r.Invoke(ctx, te.Env, call("edit_file", bad)); !res.IsError {
			t.Fatalf("expected error for %v: %+v", bad, res)
		}
	}
	// CRLF files still match LF edits from the model, and keep CRLF
	te.write(t, "crlf.txt", "one\r\ntwo\r\n")
	res = r.Invoke(ctx, te.Env, call("edit_file", map[string]any{"path": "crlf.txt", "old_string": "one\ntwo", "new_string": "1\n2"}))
	if res.IsError {
		t.Fatalf("crlf edit: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(te.Workspace, "crlf.txt")); string(b) != "1\r\n2\r\n" {
		t.Fatalf("crlf result: %q", b)
	}
}

func TestListFindSearch(t *testing.T) {
	te := newEnv(t)
	te.write(t, "main.go", "package main\nfunc Hello() {}\n")
	te.write(t, "pkg/util.go", "package pkg\nfunc Helper() {}\n")
	te.write(t, "pkg/util_test.go", "package pkg\nfunc TestHelper() {}\n")
	te.write(t, "docs/readme.md", "# Hello docs\n")
	te.write(t, "node_modules/dep/index.js", "func Hello() {}\n")
	os.WriteFile(filepath.Join(te.Workspace, "pic.bin"), []byte{0, 0, 'H', 'e', 'l', 'l', 'o'}, 0o644)
	r := fileRegistry(te)
	ctx := context.Background()

	res := r.Invoke(ctx, te.Env, call("list_dir", map[string]any{}))
	if res.IsError || !strings.Contains(res.Output, "docs/") || !strings.Contains(res.Output, "main.go") || strings.Contains(res.Output, "util.go") {
		t.Fatalf("depth 1: %s", res.Output)
	}
	res = r.Invoke(ctx, te.Env, call("list_dir", map[string]any{"depth": 2}))
	if !strings.Contains(res.Output, "  util.go") || strings.Contains(res.Output, "index.js") {
		t.Fatalf("depth 2 (node_modules must not expand): %s", res.Output)
	}
	if res = r.Invoke(ctx, te.Env, call("list_dir", map[string]any{"path": "main.go"})); !res.IsError {
		t.Fatalf("list on a file: %+v", res)
	}

	res = r.Invoke(ctx, te.Env, call("find_files", map[string]any{"pattern": "*.go"}))
	if got := strings.Split(strings.TrimSpace(res.Output), "\n"); len(got) != 3 {
		t.Fatalf("*.go at any depth: %v", got)
	}
	res = r.Invoke(ctx, te.Env, call("find_files", map[string]any{"pattern": "pkg/**/*_test.go"}))
	if strings.TrimSpace(res.Output) != "pkg/util_test.go" {
		t.Fatalf("** glob: %q", res.Output)
	}
	res = r.Invoke(ctx, te.Env, call("find_files", map[string]any{"pattern": "*.{md,bin}"}))
	if !strings.Contains(res.Output, "readme.md") || !strings.Contains(res.Output, "pic.bin") {
		t.Fatalf("brace glob: %q", res.Output)
	}
	if res = r.Invoke(ctx, te.Env, call("find_files", map[string]any{"pattern": "*.zzz"})); !strings.Contains(res.Output, "No files match") {
		t.Fatalf("no match: %q", res.Output)
	}
	if res = r.Invoke(ctx, te.Env, call("find_files", map[string]any{"pattern": "[bad"})); !res.IsError {
		t.Fatalf("bad glob: %+v", res)
	}

	res = r.Invoke(ctx, te.Env, call("search_text", map[string]any{"pattern": "func (Hello|Helper)"}))
	if !strings.Contains(res.Output, "main.go:2:") || !strings.Contains(res.Output, "pkg/util.go:2:") || strings.Contains(res.Output, "node_modules") || strings.Contains(res.Output, "pic.bin") {
		t.Fatalf("search: %s", res.Output)
	}
	res = r.Invoke(ctx, te.Env, call("search_text", map[string]any{"pattern": "hello", "ignore_case": true, "glob": "*.md"}))
	if !strings.Contains(res.Output, "docs/readme.md:1:") || strings.Contains(res.Output, "main.go") {
		t.Fatalf("glob+ignore_case: %s", res.Output)
	}
	res = r.Invoke(ctx, te.Env, call("search_text", map[string]any{"pattern": "func", "max_results": 1}))
	if !strings.Contains(res.Output, "stopped at max_results=1") {
		t.Fatalf("max_results: %s", res.Output)
	}
	if res = r.Invoke(ctx, te.Env, call("search_text", map[string]any{"pattern": "(unclosed"})); !res.IsError || !strings.Contains(res.Output, "invalid regular expression") {
		t.Fatalf("bad regex: %+v", res)
	}
	if res = r.Invoke(ctx, te.Env, call("search_text", map[string]any{"pattern": "package", "path": "main.go"})); !strings.Contains(res.Output, "main.go:1:") {
		t.Fatalf("single file: %s", res.Output)
	}
}
