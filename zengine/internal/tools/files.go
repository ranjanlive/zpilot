package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// rel shortens a path for display when it lies inside the workspace.
func (e *Env) rel(p string) string {
	if r, err := filepath.Rel(e.Workspace, p); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return p
}

// extMIME covers the media types the engine can attach. Extension wins over
// sniffing because Windows' registry-backed mime tables are unreliable.
var extMIME = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp",
	".gif": "image/gif", ".bmp": "image/bmp", ".heic": "image/heic", ".heif": "image/heif",
	".wav": "audio/wav", ".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/opus", ".flac": "audio/flac",
	".mp4": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm", ".avi": "video/x-msvideo",
	".mpeg": "video/mpeg", ".mpg": "video/mpeg", ".wmv": "video/x-ms-wmv", ".3gp": "video/3gpp", ".flv": "video/x-flv",
	".pdf": "application/pdf",
}

func detectMIME(path string, head []byte) string {
	if m, ok := extMIME[strings.ToLower(filepath.Ext(path))]; ok {
		return m
	}
	if m := mime.TypeByExtension(filepath.Ext(path)); m != "" && !strings.HasPrefix(m, "text/") {
		return strings.Split(m, ";")[0]
	}
	return strings.Split(http.DetectContentType(head), ";")[0]
}

func looksBinary(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	return !utf8.Valid(b[:min(len(b), 8192)]) && !utf8.Valid(trimPartialRune(b[:min(len(b), 8192)]))
}

// trimPartialRune drops an incomplete rune at the end of a cut buffer.
func trimPartialRune(b []byte) []byte {
	for i := 1; i <= 3 && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			break
		}
	}
	return b
}

// ---- view_file ----

type viewFile struct{}

func (viewFile) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "view_file",
		Description: "Read a file. Text files are returned with line numbers (use start_line/end_line for large files). " +
			"Images, audio, video and PDFs are attached so you can see/hear/read them, if the configured model supports that media type; " +
			"otherwise the result says so.",
		Schema: schema(map[string]any{
			"path":       str("File path, absolute or relative to the workspace."),
			"start_line": num("First line to return, 1-based (text files only)."),
			"end_line":   num("Last line to return, inclusive (text files only)."),
		}, "path"),
	}
}

func (viewFile) Run(ctx context.Context, env *Env, raw json.RawMessage) Result {
	var a struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	_ = json.Unmarshal(raw, &a)
	p, err := env.ResolvePath(a.Path, false)
	if err != nil {
		return Fail("%v", err)
	}
	info, err := os.Stat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Fail("file not found: %s. Use list_dir or find_files to locate it.", env.rel(p))
	case err != nil:
		return Fail("cannot read %s: %v", env.rel(p), err)
	case info.IsDir():
		return Fail("%s is a directory; use list_dir to see its contents.", env.rel(p))
	}
	f, err := os.Open(p)
	if err != nil {
		return Fail("cannot open %s: %v", env.rel(p), err)
	}
	defer f.Close()
	head := make([]byte, 8192)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	mt := detectMIME(p, head)

	if kind, ok := llm.KindForMIME(mt); ok {
		return viewMedia(env, p, f, info, kind, mt)
	}
	// Only the binary check needs the header; text is re-read from the start below.
	if looksBinary(head) {
		return Fail("%s is a binary file (%s, %d bytes) that cannot be shown as text.", env.rel(p), mt, info.Size())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Fail("cannot read %s: %v", env.rel(p), err)
	}
	return viewText(ctx, env, p, f, info.Size(), a.StartLine, a.EndLine)
}

func viewMedia(env *Env, p string, f *os.File, info os.FileInfo, kind llm.MediaKind, mt string) Result {
	if !env.Caps.Supports(kind) {
		return Fail("%s: %s", env.rel(p), env.Caps.UnsupportedMessage(kind))
	}
	limit := int64(env.Cfg.Tools.MaxAttachMB * 1024 * 1024)
	if limit > 0 && info.Size() > limit {
		return Fail("%s is %s, above the %s attachment limit (tools.max_attach_mb). Use a smaller or downscaled copy.",
			env.rel(p), llm.HumanBytes(info.Size()), llm.HumanBytes(limit))
	}
	if bl := int64(env.Cfg.BodyLimitBytes()); bl > 0 && info.Size()*4/3 > bl*6/10 {
		return Fail("%s is %s; attached inline it would take more than 60%% of the %s request limit. Use a smaller or downscaled copy.",
			env.rel(p), llm.HumanBytes(info.Size()), llm.HumanBytes(bl))
	}
	// The caller already consumed a header chunk to sniff the type; start over.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Fail("cannot read %s: %v", env.rel(p), err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return Fail("cannot read %s: %v", env.rel(p), err)
	}
	if kind == llm.MediaImage {
		if err := llm.ValidateImageData(mt, data); err != nil {
			return Fail("cannot attach %s: %v", env.rel(p), err)
		}
	}
	if env.PutBlob == nil {
		return Fail("attachments are not available in this context")
	}
	blob, err := env.PutBlob(data)
	if err != nil {
		return Fail("cannot store attachment: %v", err)
	}
	m := llm.Media{Kind: kind, MIME: mt, Blob: blob, Size: int64(len(data)), Name: filepath.Base(p), Path: p}
	return Result{Output: fmt.Sprintf("Attached %s: %s (%s, %.1f KB).", kind, env.rel(p), mt, float64(len(data))/1024), Media: []llm.Media{m}}
}

const (
	defaultViewLines = 2000
	maxLineChars     = 4000
)

func viewText(ctx context.Context, env *Env, p string, r io.Reader, size int64, start, end int) Result {
	if start < 0 || end < 0 {
		return Fail("start_line and end_line must be positive")
	}
	if start == 0 {
		start = 1
	}
	if end != 0 && end < start {
		return Fail("end_line (%d) is before start_line (%d)", end, start)
	}
	limitLines := defaultViewLines
	if end != 0 {
		limitLines = end - start + 1
	}
	maxBytes := env.Cfg.Tools.MaxViewBytes
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		sb      strings.Builder
		lineNo  int
		emitted int
		more    bool
		cutByte bool
	)
	for {
		if ctx.Err() != nil {
			return Fail("cancelled")
		}
		line, err := br.ReadString('\n')
		if line == "" && err != nil {
			break
		}
		lineNo++
		if lineNo >= start {
			if emitted >= limitLines || (maxBytes > 0 && int64(sb.Len()) >= maxBytes) {
				more = true
				cutByte = maxBytes > 0 && int64(sb.Len()) >= maxBytes && emitted < limitLines
				break
			}
			text := strings.TrimRight(line, "\r\n")
			if len(text) > maxLineChars {
				text = text[:alignDown(text, maxLineChars)] + fmt.Sprintf(" … [line truncated, %d more characters]", len(text)-maxLineChars)
			}
			fmt.Fprintf(&sb, "%6d\t%s\n", lineNo, text)
			emitted++
		}
		if err != nil {
			break
		}
	}
	if emitted == 0 {
		if lineNo == 0 {
			return OK("%s is empty.", env.rel(p))
		}
		return Fail("%s has only %d line(s); start_line %d is past the end.", env.rel(p), lineNo, start)
	}
	header := fmt.Sprintf("File: %s (%d bytes). Showing lines %d-%d", env.rel(p), size, start, start+emitted-1)
	if more {
		why := "line limit"
		if cutByte {
			why = "size limit (tools.max_view_bytes)"
		}
		header += fmt.Sprintf(" — more lines follow (%s); call again with start_line=%d.", why, start+emitted)
	} else {
		header += " (end of file)."
	}
	return OK("%s\n%s", header, sb.String())
}

// ---- write_file ----

type writeFile struct{}

func (writeFile) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "write_file",
		Description: "Create a file, or overwrite it (or append to it) with the given content. Parent directories are created. Prefer edit_file for small changes to existing files.",
		Schema: schema(map[string]any{
			"path":    str("File path, absolute or relative to the workspace."),
			"content": str("Full file content."),
			"append":  flag("Append to the file instead of overwriting."),
		}, "path", "content"),
	}
}

func (writeFile) Run(_ context.Context, env *Env, raw json.RawMessage) Result {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	_ = json.Unmarshal(raw, &a)
	p, err := env.ResolvePath(a.Path, true)
	if err != nil {
		return Fail("%v", err)
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return Fail("%s is a directory", env.rel(p))
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return Fail("cannot create directory for %s: %v", env.rel(p), err)
	}
	_, existed := os.Stat(p)
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	verb := "Wrote"
	if a.Append {
		flags, verb = os.O_WRONLY|os.O_CREATE|os.O_APPEND, "Appended"
	}
	f, err := os.OpenFile(p, flags, 0o644)
	if err != nil {
		return Fail("cannot write %s: %v", env.rel(p), err)
	}
	n, werr := f.WriteString(a.Content)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return Fail("write to %s failed: %v", env.rel(p), werr)
	}
	note := ""
	if existed == nil && !a.Append {
		note = " (replaced the existing file)"
	}
	return OK("%s %d bytes to %s%s.", verb, n, env.rel(p), note)
}

// ---- edit_file ----

type editFile struct{}

func (editFile) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "edit_file",
		Description: "Replace text in an existing file. old_string must match the file exactly (including whitespace) and be unique unless replace_all is true. " +
			"Read the file first so you copy the text exactly.",
		Schema: schema(map[string]any{
			"path":        str("File path, absolute or relative to the workspace."),
			"old_string":  str("Exact text to replace."),
			"new_string":  str("Replacement text."),
			"replace_all": flag("Replace every occurrence instead of requiring a unique match."),
		}, "path", "old_string", "new_string"),
	}
}

func (editFile) Run(_ context.Context, env *Env, raw json.RawMessage) Result {
	var a struct {
		Path       string `json:"path"`
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	_ = json.Unmarshal(raw, &a)
	if a.Old == "" {
		return Fail("old_string is empty. To create or fully replace a file use write_file.")
	}
	if a.Old == a.New {
		return Fail("old_string and new_string are identical; nothing to change.")
	}
	p, err := env.ResolvePath(a.Path, true)
	if err != nil {
		return Fail("%v", err)
	}
	info, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return Fail("file not found: %s. Use write_file to create it.", env.rel(p))
	} else if err != nil {
		return Fail("cannot read %s: %v", env.rel(p), err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return Fail("cannot read %s: %v", env.rel(p), err)
	}
	content := string(data)
	old, repl := a.Old, a.New
	// Models write LF; honour the file's CRLF line endings so edits still match.
	if strings.Contains(content, "\r\n") && !strings.Contains(old, "\r") {
		old, repl = strings.ReplaceAll(old, "\n", "\r\n"), strings.ReplaceAll(repl, "\n", "\r\n")
	}
	count := strings.Count(content, old)
	switch {
	case count == 0:
		return Fail("old_string was not found in %s. It must match exactly, including indentation and line breaks. Re-read the file with view_file and copy the text again.%s", env.rel(p), nearMiss(content, old))
	case count > 1 && !a.ReplaceAll:
		return Fail("old_string matches %d places in %s. Include more surrounding lines to make it unique, or set replace_all=true.", count, env.rel(p))
	}
	replaced := 1
	updated := strings.Replace(content, old, repl, 1)
	if a.ReplaceAll {
		replaced = count
		updated = strings.ReplaceAll(content, old, repl)
	}
	if err := os.WriteFile(p, []byte(updated), info.Mode().Perm()); err != nil {
		return Fail("cannot write %s: %v", env.rel(p), err)
	}
	return OK("Edited %s: replaced %d occurrence(s).", env.rel(p), replaced)
}

// nearMiss helps the model recover from a failed match by pointing at the line
// that shares the old text's first non-blank line, if any.
func nearMiss(content, old string) string {
	first := ""
	for _, l := range strings.Split(old, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			first = t
			break
		}
	}
	if first == "" {
		return ""
	}
	for i, l := range strings.Split(content, "\n") {
		if strings.Contains(l, first) {
			return fmt.Sprintf(" The first line of old_string does appear at line %d.", i+1)
		}
	}
	return ""
}

// ---- list_dir ----

type listDir struct{}

func (listDir) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "list_dir",
		Description: "List the contents of a directory (directories first, with file sizes). Use depth to include subdirectories.",
		Schema: schema(map[string]any{
			"path":  str("Directory path; defaults to the workspace."),
			"depth": num("How many levels to show, 1-5 (default 1)."),
		}),
	}
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true, ".mypy_cache": true, ".pytest_cache": true}

func (listDir) Run(ctx context.Context, env *Env, raw json.RawMessage) Result {
	var a struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	_ = json.Unmarshal(raw, &a)
	if a.Path == "" {
		a.Path = "."
	}
	if a.Depth <= 0 {
		a.Depth = 1
	}
	a.Depth = min(a.Depth, 5)
	p, err := env.ResolvePath(a.Path, false)
	if err != nil {
		return Fail("%v", err)
	}
	info, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return Fail("directory not found: %s", env.rel(p))
	} else if err != nil {
		return Fail("cannot read %s: %v", env.rel(p), err)
	} else if !info.IsDir() {
		return Fail("%s is a file; use view_file to read it.", env.rel(p))
	}
	var sb strings.Builder
	count := 0
	const maxEntries = 500
	var walk func(dir string, depth int, indent string)
	walk = func(dir string, depth int, indent string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(&sb, "%s(cannot read: %v)\n", indent, err)
			return
		}
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		for _, e := range entries {
			if ctx.Err() != nil || count >= maxEntries {
				return
			}
			count++
			if e.IsDir() {
				fmt.Fprintf(&sb, "%s%s/\n", indent, e.Name())
				if depth > 1 && !skipDirs[e.Name()] {
					walk(filepath.Join(dir, e.Name()), depth-1, indent+"  ")
				}
				continue
			}
			size := int64(0)
			if fi, err := e.Info(); err == nil {
				size = fi.Size()
			}
			fmt.Fprintf(&sb, "%s%s  (%s)\n", indent, e.Name(), humanSize(size))
		}
	}
	walk(p, a.Depth, "")
	if count == 0 {
		return OK("%s is empty.", env.rel(p))
	}
	out := fmt.Sprintf("Directory: %s\n%s", env.rel(p), sb.String())
	if count >= maxEntries {
		out += fmt.Sprintf("… listing stopped at %d entries; narrow the path or lower depth.\n", maxEntries)
	}
	return OK("%s", out)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ---- find_files ----

type findFiles struct{}

func (findFiles) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "find_files",
		Description: "Find files by glob pattern. A pattern without '/' matches file names at any depth (\"*.go\"); " +
			"use \"**\" to cross directories (\"src/**/*_test.go\"). Skips .git, node_modules and virtual environments.",
		Schema: schema(map[string]any{
			"pattern": str("Glob pattern."),
			"path":    str("Directory to search; defaults to the workspace."),
		}, "pattern"),
	}
}

func (findFiles) Run(ctx context.Context, env *Env, raw json.RawMessage) Result {
	var a struct{ Pattern, Path string }
	_ = json.Unmarshal(raw, &a)
	if a.Path == "" {
		a.Path = "."
	}
	re, err := globRegexp(a.Pattern)
	if err != nil {
		return Fail("invalid glob pattern %q: %v", a.Pattern, err)
	}
	root, err := env.ResolvePath(a.Path, false)
	if err != nil {
		return Fail("%v", err)
	}
	matchBase := !strings.Contains(a.Pattern, "/")
	var found []string
	const maxFound = 200
	walkErr := walkTree(ctx, root, func(path string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		target := rel
		if matchBase {
			target = d.Name()
		}
		if re.MatchString(target) {
			found = append(found, env.rel(path))
			if len(found) >= maxFound {
				return io.EOF
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, io.EOF) {
		return Fail("search failed: %v", walkErr)
	}
	if len(found) == 0 {
		return OK("No files match %q under %s.", a.Pattern, env.rel(root))
	}
	sort.Strings(found)
	out := strings.Join(found, "\n")
	if len(found) >= maxFound {
		out += fmt.Sprintf("\n… stopped at %d results; use a more specific pattern or path.", maxFound)
	}
	return OK("%s", out)
}

// walkTree walks root, skipping vendor/VCS directories. Returning io.EOF from
// fn stops the walk without error.
func walkTree(ctx context.Context, root string, fn func(path string, d fs.DirEntry) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() && path != root && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		return fn(path, d)
	})
}

// globRegexp compiles a glob with ** support into an anchored regexp.
func globRegexp(glob string) (*regexp.Regexp, error) {
	if strings.TrimSpace(glob) == "" {
		return nil, errors.New("empty pattern")
	}
	var sb strings.Builder
	sb.WriteString("^")
	g := filepath.ToSlash(glob)
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
					sb.WriteString("(?:.*/)?")
				} else {
					sb.WriteString(".*")
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		case '[':
			j := strings.IndexByte(g[i:], ']')
			if j < 0 {
				return nil, errors.New("unclosed [")
			}
			class := g[i+1 : i+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			sb.WriteString("[" + class + "]")
			i += j
		case '{':
			j := strings.IndexByte(g[i:], '}')
			if j < 0 {
				return nil, errors.New("unclosed {")
			}
			alts := strings.Split(g[i+1:i+j], ",")
			for k := range alts {
				alts[k] = regexp.QuoteMeta(alts[k])
			}
			sb.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += j
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

// ---- search_text ----

type searchText struct{}

func (searchText) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "search_text",
		Description: "Search file contents with a regular expression (Go/RE2 syntax). Returns path:line: text. Skips binary files, .git, node_modules and virtual environments.",
		Schema: schema(map[string]any{
			"pattern":     str("Regular expression."),
			"path":        str("File or directory to search; defaults to the workspace."),
			"glob":        str("Only search files whose path matches this glob (for example \"*.py\")."),
			"ignore_case": flag("Case-insensitive match."),
			"max_results": num("Stop after this many matching lines (default 100, max 1000)."),
		}, "pattern"),
	}
}

func (searchText) Run(ctx context.Context, env *Env, raw json.RawMessage) Result {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignore_case"`
		MaxResults int    `json:"max_results"`
	}
	_ = json.Unmarshal(raw, &a)
	if a.Path == "" {
		a.Path = "."
	}
	if a.MaxResults <= 0 {
		a.MaxResults = 100
	}
	a.MaxResults = min(a.MaxResults, 1000)
	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Fail("invalid regular expression %q: %v", a.Pattern, err)
	}
	var globRe *regexp.Regexp
	globBase := false
	if a.Glob != "" {
		if globRe, err = globRegexp(a.Glob); err != nil {
			return Fail("invalid glob %q: %v", a.Glob, err)
		}
		globBase = !strings.Contains(a.Glob, "/")
	}
	root, err := env.ResolvePath(a.Path, false)
	if err != nil {
		return Fail("%v", err)
	}
	var sb strings.Builder
	matches, files := 0, 0
	searchFile := func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		head := make([]byte, 8192)
		n, _ := io.ReadFull(f, head)
		if looksBinary(head[:n]) {
			return nil
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil
		}
		br := bufio.NewReaderSize(f, 64*1024)
		lineNo := 0
		hit := false
		for {
			line, err := br.ReadString('\n')
			if line == "" && err != nil {
				break
			}
			lineNo++
			text := strings.TrimRight(line, "\r\n")
			if re.MatchString(text) {
				if !hit {
					files++
					hit = true
				}
				if len(text) > 300 {
					text = text[:alignDown(text, 300)] + "…"
				}
				fmt.Fprintf(&sb, "%s:%d: %s\n", env.rel(path), lineNo, text)
				matches++
				if matches >= a.MaxResults {
					return io.EOF
				}
			}
			if err != nil {
				break
			}
		}
		return nil
	}
	info, err := os.Stat(root)
	if err != nil {
		return Fail("path not found: %s", env.rel(root))
	}
	var walkErr error
	if info.IsDir() {
		walkErr = walkTree(ctx, root, func(path string, d fs.DirEntry) error {
			if d.IsDir() {
				return nil
			}
			if globRe != nil {
				target := d.Name()
				if !globBase {
					r, _ := filepath.Rel(root, path)
					target = filepath.ToSlash(r)
				}
				if !globRe.MatchString(target) {
					return nil
				}
			}
			if fi, err := d.Info(); err == nil && fi.Size() > 4<<20 {
				return nil
			}
			return searchFile(path)
		})
	} else {
		walkErr = searchFile(root)
	}
	if walkErr != nil && !errors.Is(walkErr, io.EOF) {
		return Fail("search failed: %v", walkErr)
	}
	if matches == 0 {
		return OK("No matches for %q under %s.", a.Pattern, env.rel(root))
	}
	out := fmt.Sprintf("%d matching line(s) in %d file(s):\n%s", matches, files, sb.String())
	if matches >= a.MaxResults {
		out += fmt.Sprintf("… stopped at max_results=%d; refine the pattern or path.\n", a.MaxResults)
	}
	return OK("%s", out)
}

// RegisterFileTools adds the file tools to r.
func RegisterFileTools(r *Registry) {
	for _, t := range []Tool{viewFile{}, writeFile{}, editFile{}, listDir{}, findFiles{}, searchText{}} {
		r.Register(t)
	}
}

// AttachFile loads a media file for inclusion in a user prompt, applying the
// same checks as view_file (path policy, capabilities, size limits). It
// returns the attachment, or an error message fit to show the user.
func AttachFile(env *Env, path string) (llm.Media, error) {
	p, err := env.ResolvePath(path, false)
	if err != nil {
		return llm.Media{}, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return llm.Media{}, fmt.Errorf("cannot attach %s: %w", path, err)
	}
	if info.IsDir() {
		return llm.Media{}, fmt.Errorf("cannot attach %s: it is a directory", path)
	}
	f, err := os.Open(p)
	if err != nil {
		return llm.Media{}, fmt.Errorf("cannot attach %s: %w", path, err)
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	mt := detectMIME(p, head[:n])
	kind, ok := llm.KindForMIME(mt)
	if !ok {
		return llm.Media{}, fmt.Errorf("cannot attach %s: %s is not an image, audio, video or PDF file (mention the path in your prompt and the agent will read it)", path, mt)
	}
	res := viewMedia(env, p, f, info, kind, mt)
	if res.IsError || len(res.Media) == 0 {
		return llm.Media{}, errors.New(res.Output)
	}
	return res.Media[0], nil
}
