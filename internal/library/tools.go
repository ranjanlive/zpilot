// Package library manages the shared tool and skill libraries that agents
// pick from. Tools come from the ZTOOLS registry (verified release archives)
// or from local folders; skills are folders with a SKILL.md.
//
//	<data>/library/tools/<id>/      <id>.json manifest + binary/script + .zpilot-tool.json
//	<data>/library/skills/<name>/   SKILL.md (+ any supporting files)
package library

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/store"
)

// DefaultRegistryURL is the official ZTOOLS registry.
const DefaultRegistryURL = "https://raw.githubusercontent.com/ranjanlive/ZTOOLS/main/registry.json"

// maxDownload caps a tool archive.
const maxDownload = 200 << 20

const metaFile = ".zpilot-tool.json"

// Library is rooted at <data>/library.
type Library struct {
	Root        string
	RegistryURL string
	HTTP        *http.Client
}

func New(dataDir string) *Library {
	l := &Library{Root: filepath.Join(dataDir, "library"), RegistryURL: DefaultRegistryURL, HTTP: &http.Client{Timeout: 5 * time.Minute}}
	_ = os.MkdirAll(l.ToolsDir(), 0o755)
	_ = os.MkdirAll(l.SkillsDir(), 0o755)
	return l
}

func (l *Library) ToolsDir() string  { return filepath.Join(l.Root, "tools") }
func (l *Library) SkillsDir() string { return filepath.Join(l.Root, "skills") }

// ---- installed tools ---------------------------------------------------

// ToolMeta records where an installed tool came from.
type ToolMeta struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description"`
	Category    string    `json:"category,omitempty"`
	Source      string    `json:"source"` // registry | local
	URL         string    `json:"url,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	Verified    bool      `json:"verified"`
	Installed   time.Time `json:"installed"`
	HasConfig   bool      `json:"has_config"` // a config.json the owner may need to edit
}

// Tools lists installed tools.
func (l *Library) Tools() ([]ToolMeta, error) {
	ents, err := os.ReadDir(l.ToolsDir())
	if err != nil {
		return nil, err
	}
	out := []ToolMeta{}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		m, err := l.Tool(e.Name())
		if err == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Tool returns one installed tool.
func (l *Library) Tool(id string) (ToolMeta, error) {
	if !model.ValidSlug(id) {
		return ToolMeta{}, fmt.Errorf("invalid tool id %q", id)
	}
	dir := filepath.Join(l.ToolsDir(), id)
	var m ToolMeta
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		if _, serr := os.Stat(filepath.Join(dir, id+".json")); serr != nil {
			return m, fmt.Errorf("tool %s is not installed", id)
		}
		m = ToolMeta{ID: id, Name: id, Source: "local"}
	} else if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if man, err := readManifest(filepath.Join(dir, id+".json")); err == nil && m.Description == "" {
		m.Description = man.Description
	}
	_, cerr := os.Stat(filepath.Join(dir, "config.json"))
	m.HasConfig = cerr == nil
	return m, nil
}

// ToolDir is the folder of an installed tool.
func (l *Library) ToolDir(id string) string { return filepath.Join(l.ToolsDir(), id) }

// RemoveTool uninstalls a tool from the library.
func (l *Library) RemoveTool(id string) error {
	if !model.ValidSlug(id) {
		return fmt.Errorf("invalid tool id %q", id)
	}
	if _, err := l.Tool(id); err != nil {
		return err
	}
	return os.RemoveAll(l.ToolDir(id))
}

// ToolConfig reads a tool's config.json (if it has one).
func (l *Library) ToolConfig(id string) (json.RawMessage, error) {
	if !model.ValidSlug(id) {
		return nil, fmt.Errorf("invalid tool id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(l.ToolDir(id), "config.json"))
	if err != nil {
		return nil, fmt.Errorf("tool %s has no config.json", id)
	}
	return b, nil
}

// SetToolConfig replaces a tool's config.json after checking it is a JSON object.
func (l *Library) SetToolConfig(id string, raw json.RawMessage) error {
	if _, err := l.ToolConfig(id); err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("config must be a JSON object: %w", err)
	}
	b, _ := json.MarshalIndent(obj, "", "  ")
	return store.WriteFileAtomic(filepath.Join(l.ToolDir(id), "config.json"), append(b, '\n'), 0o600)
}

// manifest is the part of an engine tool manifest we inspect.
type manifest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Command     []string `json:"command"`
}

func readManifest(p string) (manifest, error) {
	var m manifest
	b, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", filepath.Base(p), err)
	}
	if strings.TrimSpace(m.Description) == "" || len(m.Command) == 0 {
		return m, fmt.Errorf("%s: a manifest needs a description and a command", filepath.Base(p))
	}
	return m, nil
}

// InstallLocal copies a tool folder (containing <id>.json) into the library.
func (l *Library) InstallLocal(src string) (ToolMeta, error) {
	src, err := filepath.Abs(src)
	if err != nil {
		return ToolMeta{}, err
	}
	id := filepath.Base(src)
	if !model.ValidSlug(id) {
		return ToolMeta{}, fmt.Errorf("folder name %q is not a valid tool id (lowercase letters, digits, _ and -)", id)
	}
	man, err := readManifest(filepath.Join(src, id+".json"))
	if err != nil {
		return ToolMeta{}, fmt.Errorf("%s does not look like a tool folder: %w", src, err)
	}
	tmp, err := os.MkdirTemp(l.ToolsDir(), ".install-*")
	if err != nil {
		return ToolMeta{}, err
	}
	defer os.RemoveAll(tmp)
	if err := CopyDir(src, tmp); err != nil {
		return ToolMeta{}, err
	}
	meta := ToolMeta{ID: id, Name: id, Description: man.Description, Source: "local", URL: src, Installed: time.Now().UTC()}
	return meta, l.commit(tmp, meta)
}

// commit writes the metadata and atomically swaps the tool folder in.
func (l *Library) commit(staged string, meta ToolMeta) error {
	if err := store.WriteJSONAtomic(filepath.Join(staged, metaFile), meta); err != nil {
		return err
	}
	dst := l.ToolDir(meta.ID)
	old := dst + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			return fmt.Errorf("replace existing tool: %w", err)
		}
	}
	if err := os.Rename(staged, dst); err != nil {
		_ = os.Rename(old, dst)
		return err
	}
	_ = os.RemoveAll(old)
	return nil
}

// ---- registry ----------------------------------------------------------

// Registry is the ZTOOLS registry.json (the fields ZPilot uses).
type Registry struct {
	Name       string          `json:"name"`
	UpdatedAt  string          `json:"updated_at"`
	Categories []RegCategory   `json:"categories"`
	Tools      []RegistryEntry `json:"tools"`
}

type RegCategory struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type RegistryEntry struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Category    string            `json:"category"`
	License     string            `json:"license"`
	Icon        string            `json:"icon"`
	Tags        []string          `json:"tags"`
	Suite       string            `json:"suite,omitempty"`
	SourceURL   string            `json:"source_url"`
	Platforms   []string          `json:"platforms"`
	Downloads   map[string]string `json:"downloads"`
	SHA256      map[string]string `json:"sha256,omitempty"` // optional; otherwise SHA256SUMS.txt is used
	Installed   string            `json:"installed_version,omitempty"`
}

// Platform is this machine's registry platform key, e.g. windows-amd64.
func Platform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// FetchRegistry downloads and parses the registry.
func (l *Library) FetchRegistry(ctx context.Context) (*Registry, error) {
	b, err := l.get(ctx, l.RegistryURL, 8<<20)
	if err != nil {
		return nil, fmt.Errorf("fetch registry: %w", err)
	}
	var r Registry
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("registry is not valid JSON: %w", err)
	}
	for i := range r.Tools {
		if m, err := l.Tool(r.Tools[i].ID); err == nil {
			r.Tools[i].Installed = m.Version
		}
	}
	return &r, nil
}

// InstallOptions control a registry install.
type InstallOptions struct {
	// AllowUnverified installs even when no checksum is published.
	AllowUnverified bool
}

// Install downloads a registry tool for this platform, verifies its checksum
// and installs it into the library.
func (l *Library) Install(ctx context.Context, id string, opt InstallOptions) (ToolMeta, error) {
	if !model.ValidSlug(id) {
		return ToolMeta{}, fmt.Errorf("invalid tool id %q", id)
	}
	reg, err := l.FetchRegistry(ctx)
	if err != nil {
		return ToolMeta{}, err
	}
	var e *RegistryEntry
	for i := range reg.Tools {
		if reg.Tools[i].ID == id {
			e = &reg.Tools[i]
		}
	}
	if e == nil {
		return ToolMeta{}, fmt.Errorf("tool %q is not in the registry", id)
	}
	plat := Platform()
	url := e.Downloads[plat]
	if url == "" {
		return ToolMeta{}, fmt.Errorf("%s has no build for %s (available: %s)", id, plat, strings.Join(keys(e.Downloads), ", "))
	}
	archive, err := l.get(ctx, url, maxDownload)
	if err != nil {
		return ToolMeta{}, fmt.Errorf("download %s: %w", url, err)
	}
	sum := sha256.Sum256(archive)
	got := hex.EncodeToString(sum[:])
	want := e.SHA256[plat]
	if want == "" {
		want, _ = l.publishedSum(ctx, url)
	}
	verified := false
	switch {
	case want != "" && !strings.EqualFold(want, got):
		return ToolMeta{}, fmt.Errorf("checksum mismatch for %s: published %s, downloaded %s. The download is corrupt or was tampered with; nothing was installed", path.Base(url), want, got)
	case want != "":
		verified = true
	case !opt.AllowUnverified:
		return ToolMeta{}, fmt.Errorf("%s publishes no checksum, so the download cannot be verified. Install anyway only if you trust the source (allow_unverified)", id)
	}
	tmp, err := os.MkdirTemp(l.ToolsDir(), ".install-*")
	if err != nil {
		return ToolMeta{}, err
	}
	defer os.RemoveAll(tmp)
	if err := extractTool(archive, id, tmp); err != nil {
		return ToolMeta{}, err
	}
	if _, err := readManifest(filepath.Join(tmp, id+".json")); err != nil {
		return ToolMeta{}, err
	}
	meta := ToolMeta{ID: id, Name: e.Name, Version: e.Version, Description: e.Description, Category: e.Category,
		Source: "registry", URL: url, SHA256: got, Verified: verified, Installed: time.Now().UTC()}
	return meta, l.commit(tmp, meta)
}

// publishedSum looks the archive up in SHA256SUMS.txt next to it.
func (l *Library) publishedSum(ctx context.Context, archiveURL string) (string, error) {
	base := archiveURL[:strings.LastIndex(archiveURL, "/")+1]
	b, err := l.get(ctx, base+"SHA256SUMS.txt", 1<<20)
	if err != nil {
		return "", err
	}
	name := path.Base(archiveURL)
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", errors.New("not listed")
}

func (l *Library) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	if strings.HasPrefix(url, "file://") {
		return os.ReadFile(strings.TrimPrefix(url, "file://"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "zpilot")
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response larger than %d MB", limit>>20)
	}
	return b, nil
}

// extractTool finds the folder holding <id>.json in the archive (at the root
// or as <id>/<id>.json, ignoring template-only agent_tools/ folders) and
// extracts that folder's files into dst. Paths are validated so an archive
// cannot write outside dst.
func extractTool(archive []byte, id, dst string) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("not a zip archive: %w", err)
	}
	prefix := ""
	found := false
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if strings.HasPrefix(name, "agent_tools/") {
			continue
		}
		if path.Base(name) == id+".json" {
			dir := path.Dir(name)
			if dir == "." {
				dir = ""
			}
			if !found || len(dir) < len(prefix) {
				prefix, found = dir, true
			}
		}
	}
	if !found {
		return fmt.Errorf("archive has no %s.json manifest", id)
	}
	if prefix != "" {
		prefix += "/"
	}
	var total int64
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, "/") {
			continue
		}
		clean := path.Clean(strings.TrimPrefix(name, prefix))
		if clean == "." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) || strings.Contains(clean, ":") {
			return fmt.Errorf("archive contains an unsafe path: %s", f.Name)
		}
		total += int64(f.UncompressedSize64)
		if total > 2*maxDownload {
			return errors.New("archive expands to more than 400 MB")
		}
		out := filepath.Join(dst, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if f.Mode()&0o111 != 0 || isLikelyBinary(clean, id) {
			mode = 0o755
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(w, io.LimitReader(rc, int64(f.UncompressedSize64)+1))
		rc.Close()
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// isLikelyBinary marks the tool's own executable runnable on Unix even when
// the archive (often built on Windows) lost the permission bits.
func isLikelyBinary(rel, id string) bool {
	b := path.Base(rel)
	return b == id || b == id+".exe" || strings.HasSuffix(b, ".sh")
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CopyDir copies a directory tree (regular files and folders only).
func CopyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks and devices
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm()|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
