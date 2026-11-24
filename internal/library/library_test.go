package library

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkzip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

const demoManifest = `{"name":"demo","description":"Demo tool.","command":["{dir}/demo"]}`

func TestExtractRejectsPathTraversal(t *testing.T) {
	for _, bad := range []string{"../evil.txt", "demo/../../evil.txt", "/abs/evil", "C:/evil"} {
		z := mkzip(t, map[string]string{"demo.json": demoManifest, bad: "x"})
		dst := t.TempDir()
		if err := extractTool(z, "demo", dst); err == nil {
			t.Errorf("archive with %q was accepted", bad)
		}
	}
}

func TestExtractLayouts(t *testing.T) {
	// root layout
	dst := t.TempDir()
	if err := extractTool(mkzip(t, map[string]string{"demo.json": demoManifest, "demo.exe": "bin", "README.md": "r"}), "demo", dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "demo.exe")); err != nil {
		t.Fatal("binary not extracted")
	}
	// suite layout: pick <id>/<id>.json and ignore the agent_tools templates
	dst = t.TempDir()
	z := mkzip(t, map[string]string{"agent_tools/demo/demo.json": demoManifest, "demo/demo.json": demoManifest, "demo/config.json": "{}", "other/other.json": "{}"})
	if err := extractTool(z, "demo", dst); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"demo.json", "config.json"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Fatalf("%s missing", want)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "other")); err == nil {
		t.Fatal("unrelated tool extracted")
	}
}

// Registry install verifies the archive against SHA256SUMS.txt.
func TestInstallVerifiesChecksum(t *testing.T) {
	archive := mkzip(t, map[string]string{"demo.json": demoManifest, "demo": "bin"})
	sum := sha256.Sum256(archive)
	good := hex.EncodeToString(sum[:])
	published := good
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Registry{Tools: []RegistryEntry{{ID: "demo", Name: "Demo", Version: "1.0.0",
			Downloads: map[string]string{Platform(): srv.URL + "/rel/demo.zip"}}}})
	})
	mux.HandleFunc("/rel/demo.zip", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/rel/SHA256SUMS.txt", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "%s  demo.zip\n", published) })
	srv = httptest.NewServer(mux)
	defer srv.Close()

	l := New(t.TempDir())
	l.RegistryURL = srv.URL + "/registry.json"
	published = strings.Repeat("0", 64)
	if _, err := l.Install(context.Background(), "demo", InstallOptions{}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered archive installed: %v", err)
	}
	if _, err := l.Tool("demo"); err == nil {
		t.Fatal("tool left installed after a failed verification")
	}
	published = good
	m, err := l.Install(context.Background(), "demo", InstallOptions{})
	if err != nil || !m.Verified {
		t.Fatalf("install: %+v %v", m, err)
	}
	if tools, _ := l.Tools(); len(tools) != 1 || tools[0].Version != "1.0.0" {
		t.Fatalf("library: %+v", tools)
	}
}

func TestSkills(t *testing.T) {
	l := New(t.TempDir())
	if err := l.SaveSkill(Skill{Name: "../x", Description: "d", Body: "b"}); err == nil {
		t.Fatal("unsafe skill name accepted")
	}
	if err := l.SaveSkill(Skill{Name: "notes", Description: "Use for notes", Body: "Be brief."}); err != nil {
		t.Fatal(err)
	}
	s, err := l.Skill("notes", true)
	if err != nil || s.Description != "Use for notes" || !strings.Contains(s.Body, "Be brief.") {
		t.Fatalf("skill: %+v %v", s, err)
	}
}
