// Package fs gives Alfred read-only access to the folder a chat was opened in.
package fs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

const (
	maxRead   = 8 << 10
	maxList   = 200
	maxScan   = 1 << 20
	maxVisits = 4000
	maxSearch = 16 << 20
)

// Config: Home anchors the always-denied credential folders; Deny adds exact files such as the vault.
type Config struct {
	Home string
	Deny []string
}

var (
	deniedDirs  = []string{".ssh", ".gnupg", ".aws", ".haythos", ".kube", ".docker", ".azure", ".terraform.d", ".claude", ".codex"}
	deniedHome  = [][]string{{".config", "gh"}, {".config", "gcloud"}}
	deniedNames = []string{
		".netrc", ".npmrc", ".pypirc", ".git-credentials", ".envrc", ".pgpass", ".my.cnf",
		".bash_history", ".zsh_history", ".python_history", ".node_repl_history", ".psql_history",
	}
	deniedExts = []string{".pem", ".key", ".p12", ".pfx", ".enc", ".env", ".tfstate", ".tfstate.backup"}

	// A file with one of these extensions is a trusted source; anything else a read returns is untrusted data.
	sourceExts = map[string]bool{
		".go": true, ".py": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true,
		".java": true, ".kt": true, ".kts": true, ".scala": true, ".rs": true, ".c": true, ".h": true, ".cc": true,
		".cpp": true, ".hpp": true, ".cs": true, ".swift": true, ".rb": true, ".php": true, ".lua": true, ".dart": true,
		".sh": true, ".bash": true, ".zsh": true, ".sql": true, ".proto": true, ".ex": true, ".exs": true, ".erl": true,
		".hs": true, ".clj": true, ".jl": true, ".vue": true, ".svelte": true,
	}
)

type tool struct{ cfg Config }

func Register(r *local.Registry, cfg Config) {
	t := tool{cfg: cfg}
	pathArg := `{"type":"object","properties":{"path":{"type":"string","description":"path relative to the chat folder"}`
	r.Register(task.ToolSpec{
		Name:        "fs.list",
		Description: "List files and folders in the folder this chat was opened in (or a subfolder). Read-only.",
		InputSchema: json.RawMessage(pathArg + `}}`),
	}, t.list)
	r.Register(task.ToolSpec{
		Name:        "fs.read",
		Description: "Read up to 8 KiB from a text file. Use fs.grep first and pass a 1-based start_line and end_line for relevant lines.",
		InputSchema: json.RawMessage(pathArg + `,"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["path"]}`),
	}, t.read)
	r.Register(task.ToolSpec{
		Name:        "fs.glob",
		Description: "Find file paths recursively by a shell pattern on the filename, for example *.go. Optional path narrows the folder.",
		InputSchema: json.RawMessage(pathArg + `,"pattern":{"type":"string"}},"required":["pattern"]}`),
	}, func(ctx context.Context, raw []byte) (task.ToolResult, error) { return t.search(ctx, raw, false) })
	r.Register(task.ToolSpec{
		Name:        "fs.grep",
		Description: "Find up to 32 lines containing a literal string. Returns path:line:text. Use path and filename pattern to narrow the search.",
		InputSchema: json.RawMessage(pathArg + `,"query":{"type":"string"},"pattern":{"type":"string"}},"required":["query"]}`),
	}, func(ctx context.Context, raw []byte) (task.ToolResult, error) { return t.search(ctx, raw, true) })
}

func refuse(msg string) (task.ToolResult, error) {
	return task.ToolResult{Content: msg, IsError: true}, nil
}

type args struct {
	Path string `json:"path"`
}

type scopedPath struct {
	root, full, rel string
}

func (p scopedPath) openDir() (*os.Root, error) {
	expected, err := os.Lstat(p.full)
	if err != nil || !expected.IsDir() {
		return nil, errors.New("not a readable folder")
	}
	chat, err := os.OpenRoot(p.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = chat.Close() }()
	dir, err := chat.OpenRoot(p.rel)
	if err != nil {
		return nil, err
	}
	if opened, err := dir.Stat("."); err != nil || !os.SameFile(opened, expected) {
		_ = dir.Close()
		return nil, errors.New("folder changed while opening it")
	}
	return dir, nil
}

// resolve returns the real path of rel inside the chat folder, or a reason the model may read.
func (t tool) resolve(ctx context.Context, raw []byte) (scopedPath, string) {
	root := app.WorkdirFrom(ctx)
	if root == "" {
		return scopedPath{}, "this chat has no folder; file tools only work in `haythos chat`"
	}
	var a args
	if len(raw) > 0 && json.Unmarshal(raw, &a) != nil {
		return scopedPath{}, "arguments must be {\"path\": \"...\"}"
	}
	if a.Path == "" {
		a.Path = "."
	}
	if filepath.IsAbs(a.Path) {
		return scopedPath{}, "use a path relative to the chat folder"
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return scopedPath{}, "the chat folder is not readable"
	}
	full, err := filepath.EvalSymlinks(filepath.Join(realRoot, a.Path))
	if errors.Is(err, os.ErrNotExist) {
		return scopedPath{}, a.Path + ": not found"
	}
	if err != nil {
		return scopedPath{}, a.Path + ": not readable"
	}
	if full, err = onDisk(full); err != nil {
		return scopedPath{}, a.Path + ": not readable"
	}
	if realRoot, err = onDisk(realRoot); err != nil {
		return scopedPath{}, "the chat folder is not readable"
	}
	rel, err := filepath.Rel(realRoot, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return scopedPath{}, a.Path + ": outside the chat folder"
	}
	if t.denied(full) {
		return scopedPath{}, a.Path + ": not allowed (credential file)"
	}
	return scopedPath{root: realRoot, full: full, rel: rel}, ""
}

// onDisk rewrites each component of an absolute, symlink-free path to the name stored on disk, because case-insensitive file systems (APFS) open ".ſsh" or ".SSH" as ".ssh".
func onDisk(full string) (string, error) {
	out := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(full, out), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		name, err := storedName(out, part)
		if err != nil {
			return "", err
		}
		out = filepath.Join(out, name)
	}
	return out, nil
}

func storedName(dir, typed string) (string, error) {
	want, err := os.Lstat(filepath.Join(dir, typed))
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name() == typed {
			return typed, nil
		}
	}
	for _, e := range entries {
		if fi, err := os.Lstat(filepath.Join(dir, e.Name())); err == nil && os.SameFile(fi, want) {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("stored name of %s: %w", typed, os.ErrNotExist)
}

func (t tool) denied(full string) bool {
	for _, d := range t.cfg.Deny {
		if real, err := filepath.EvalSymlinks(d); err == nil && strings.EqualFold(real, full) {
			return true
		}
	}
	parts := splitLower(full)
	if t.cfg.Home != "" {
		if home, err := filepath.EvalSymlinks(t.cfg.Home); err == nil {
			if underHome(full, home) {
				rel := parts[len(splitLower(home)):]
				for _, d := range deniedHome {
					if hasPrefix(rel, d) {
						return true
					}
				}
			}
		}
	}
	for i, part := range parts {
		for _, d := range deniedDirs {
			if part == d {
				return true
			}
		}
		if part == ".git" && i == len(parts)-2 && parts[i+1] == "config" {
			return true
		}
	}
	return deniedName(filepath.Base(full))
}

func splitLower(p string) []string {
	return strings.Split(strings.ToLower(p), string(filepath.Separator))
}

func hasPrefix(parts, prefix []string) bool {
	if len(parts) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if parts[i] != p {
			return false
		}
	}
	return true
}

func underHome(full, home string) bool {
	h := strings.ToLower(home)
	f := strings.ToLower(full)
	return f == h || strings.HasPrefix(f, h+string(filepath.Separator))
}

func deniedName(name string) bool {
	name = strings.ToLower(name)
	if name == ".env" || strings.HasPrefix(name, ".env.") || strings.HasPrefix(name, "id_") {
		return true
	}
	for _, n := range deniedNames {
		if name == n {
			return true
		}
	}
	for _, e := range deniedExts {
		if strings.HasSuffix(name, e) {
			return true
		}
	}
	return false
}

func (t tool) read(ctx context.Context, raw []byte) (task.ToolResult, error) {
	target, why := t.resolve(ctx, raw)
	if why != "" {
		return refuse(why)
	}
	var a struct {
		Start int `json:"start_line"`
		End   int `json:"end_line"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Start < 0 || a.End < 0 || (a.End > 0 && a.End < a.Start) {
		return refuse("use a valid 1-based line range")
	}
	resolved, err := os.Lstat(target.full)
	if err != nil || !resolved.Mode().IsRegular() {
		return refuse("not a regular file")
	}
	root, err := os.OpenRoot(target.root)
	if err != nil {
		return refuse("not readable")
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(target.rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return refuse("not readable")
	}
	defer func() { _ = f.Close() }()
	if opened, err := f.Stat(); err != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, resolved) {
		return refuse("not readable")
	}
	var b []byte
	var errRead error
	if a.Start > 0 || a.End > 0 {
		b, errRead = readLines(f, max(1, a.Start), a.End)
	} else {
		b, errRead = io.ReadAll(io.LimitReader(f, maxRead+1))
	}
	if errRead != nil {
		return refuse(errRead.Error())
	}
	truncated := len(b) > maxRead
	if truncated {
		b = b[:maxRead]
		for i := 0; i < utf8.UTFMax-1 && len(b) > 0 && !utf8.Valid(b); i++ {
			b = b[:len(b)-1]
		}
	}
	if !utf8.Valid(b) {
		return refuse("not text (binary file)")
	}
	out := string(b)
	if truncated {
		out += fmt.Sprintf("\n[truncated at %d KiB; use fs.grep or request a smaller line range]", maxRead>>10)
	}
	return task.ToolResult{Content: task.StripInvisible(out), Untrusted: !sourceExts[strings.ToLower(filepath.Ext(target.full))]}, nil
}

func readLines(f io.Reader, start, end int) ([]byte, error) {
	bounded := &io.LimitedReader{R: f, N: maxScan + 1}
	scanner := bufio.NewScanner(bounded)
	var out []byte
	for line := 1; scanner.Scan(); line++ {
		if line < start {
			continue
		}
		if line > start {
			out = append(out, '\n')
		}
		out = append(out, scanner.Bytes()[:min(len(scanner.Bytes()), maxRead+1-len(out))]...)
		if line == end || len(out) > maxRead {
			return out, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("cannot read requested lines: %w", err)
	}
	if bounded.N == 0 && len(out) <= maxRead {
		return nil, errors.New("requested range exceeds the 1 MiB scan limit")
	}
	return out, nil
}

func (t tool) search(ctx context.Context, raw []byte, grep bool) (task.ToolResult, error) {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Query   string `json:"query"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || (grep && a.Query == "") || (!grep && a.Pattern == "") {
		return refuse("provide a search query or filename pattern")
	}
	if a.Pattern == "" {
		a.Pattern = "*"
	}
	if _, err := filepath.Match(a.Pattern, ""); err != nil {
		return refuse("invalid filename pattern")
	}
	target, why := t.resolve(ctx, raw)
	if why != "" {
		return refuse(why)
	}
	root, err := target.openDir()
	if err != nil {
		return refuse("not a readable folder")
	}
	defer func() { _ = root.Close() }()
	var out strings.Builder
	untrusted := false
	limit := maxList
	if grep {
		limit = 32
	}
	visits := 0
	scanBudget := int64(maxSearch)
	partial := false
	err = iofs.WalkDir(root.FS(), ".", func(path string, entry iofs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		visits++
		if visits > maxVisits || limit <= 0 || out.Len() >= maxRead || scanBudget == 0 {
			return iofs.SkipAll
		}
		if path == "." {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || t.denied(filepath.Join(target.full, path)) {
				return iofs.SkipDir
			}
			return nil
		}
		matched, _ := filepath.Match(a.Pattern, entry.Name())
		if !matched || !entry.Type().IsRegular() || t.denied(filepath.Join(target.full, path)) {
			return nil
		}
		displayPath := filepath.ToSlash(filepath.Join(a.Path, path))
		if !grep {
			out.WriteString(displayPath + "\n")
			limit--
			return nil
		}
		matchedText, incomplete := grepFile(ctx, root, path, displayPath, entry, a.Query, &out, &limit, &scanBudget)
		partial = partial || incomplete
		if matchedText && !sourceExts[strings.ToLower(filepath.Ext(path))] {
			untrusted = true
		}
		return nil
	})
	if err != nil {
		return refuse("search stopped: " + err.Error())
	}
	text := out.String()
	if text == "" {
		text = "No matches."
	}
	if len(text) > maxRead || limit == 0 || visits > maxVisits || scanBudget == 0 || partial {
		text = strings.ToValidUTF8(text[:min(len(text), maxRead)], "") + "\n[search incomplete; narrow path or pattern]"
	}
	return task.ToolResult{Content: task.StripInvisible(strings.TrimSuffix(text, "\n")), Untrusted: untrusted}, nil
}

func grepFile(ctx context.Context, root *os.Root, path, displayPath string, entry iofs.DirEntry, query string, out *strings.Builder, limit *int, scanBudget *int64) (bool, bool) {
	info, err := entry.Info()
	if err != nil || !info.Mode().IsRegular() {
		return false, true
	}
	f, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, true
	}
	defer func() { _ = f.Close() }()
	if opened, err := f.Stat(); err != nil || !os.SameFile(opened, info) {
		return false, true
	}
	readLimit := min(int64(maxScan+1), *scanBudget)
	bounded := &io.LimitedReader{R: f, N: readLimit}
	scanner := bufio.NewScanner(bounded)
	scanner.Buffer(make([]byte, 4096), maxScan+2)
	matched := false
	for line := 1; *limit > 0 && out.Len() < maxRead && ctx.Err() == nil && scanner.Scan(); line++ {
		text := scanner.Text()
		if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			break
		}
		if index := strings.Index(text, query); index >= 0 {
			start := max(0, index-80)
			text = strings.ToValidUTF8(text[start:min(len(text), start+240)], "")
			fmt.Fprintf(out, "%s:%d:%s\n", displayPath, line, text)
			(*limit)--
			matched = true
		}
	}
	*scanBudget -= readLimit - bounded.N
	return matched, bounded.N == 0 || scanner.Err() != nil
}

func (t tool) list(ctx context.Context, raw []byte) (task.ToolResult, error) {
	target, why := t.resolve(ctx, raw)
	if why != "" {
		return refuse(why)
	}
	dir, err := target.openDir()
	if err != nil {
		return refuse("not a readable folder")
	}
	defer func() { _ = dir.Close() }()
	entries, err := iofs.ReadDir(dir.FS(), ".")
	if err != nil {
		return refuse("not a readable folder")
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if t.denied(filepath.Join(target.full, name)) {
				continue
			}
			name += "/"
		} else if t.denied(filepath.Join(target.full, name)) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	more := ""
	if len(names) > maxList {
		more = fmt.Sprintf("\n[%d more not shown]", len(names)-maxList)
		names = names[:maxList]
	}
	return task.ToolResult{Content: task.StripInvisible(strings.Join(names, "\n") + more)}, nil
}
