// Package readfile gives Alfred read-only access to the folder a chat was opened in.
package readfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

const (
	maxList = 200
)

// Config: Home anchors the always-denied credential folders; Deny adds exact files such as the vault.
type Config struct {
	Home string
	Deny []string
	Rg   string
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

// Guard scopes paths to the chat folder and refuses credential files; gitread reuses it.
type Guard struct{ cfg Config }

func NewGuard(cfg Config) Guard { return Guard{cfg: cfg} }

func IsSource(name string) bool { return sourceExts[strings.ToLower(filepath.Ext(name))] }

const pathArg = `{"type":"object","properties":{"path":{"type":"string","description":"path relative to the chat folder"}`

func Register(r *local.Registry, cfg Config) {
	t := Guard{cfg: cfg}
	r.Register(task.ToolSpec{
		Name:        "file.list",
		Description: "List files and folders in the folder this chat was opened in (or a subfolder). Read-only.",
		InputSchema: json.RawMessage(pathArg + `}}`),
	}, t.list)
	r.Register(readSpec, t.read)
	r.Register(searchSpec, t.search)
}

func refuse(msg string) (task.ToolResult, error) {
	return task.ToolResult{Content: msg, IsError: true}, nil
}

type args struct {
	Path string `json:"path"`
}

// Path is a resolved location inside the chat folder: Full is absolute, Rel is relative to Root.
type Path struct {
	Root, Full, Rel string
}

func (p Path) openDir() (*os.Root, error) {
	expected, err := os.Lstat(p.Full)
	if err != nil || !expected.IsDir() {
		return nil, errors.New("not a readable folder")
	}
	chat, err := os.OpenRoot(p.Root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = chat.Close() }()
	dir, err := chat.OpenRoot(p.Rel)
	if err != nil {
		return nil, err
	}
	if opened, err := dir.Stat("."); err != nil || !os.SameFile(opened, expected) {
		_ = dir.Close()
		return nil, errors.New("folder changed while opening it")
	}
	return dir, nil
}

func (t Guard) resolve(ctx context.Context, raw []byte) (Path, string) {
	var a args
	if len(raw) > 0 && json.Unmarshal(raw, &a) != nil {
		return Path{}, "arguments must be {\"path\": \"...\"}"
	}
	return t.Resolve(ctx, a.Path)
}

// Resolve returns the real path of path inside the chat folder, or a reason the model may read.
func (t Guard) Resolve(ctx context.Context, path string) (Path, string) {
	root := app.WorkdirFrom(ctx)
	if root == "" {
		return Path{}, "this chat has no folder; file tools only work in `haythos chat`"
	}
	a := args{Path: path}
	if a.Path == "" {
		a.Path = "."
	}
	if filepath.IsAbs(a.Path) {
		return Path{}, "use a path relative to the chat folder"
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Path{}, "the chat folder is not readable"
	}
	full, err := filepath.EvalSymlinks(filepath.Join(realRoot, a.Path))
	if errors.Is(err, os.ErrNotExist) {
		return Path{}, a.Path + ": not found"
	}
	if err != nil {
		return Path{}, a.Path + ": not readable"
	}
	if full, err = onDisk(full); err != nil {
		return Path{}, a.Path + ": not readable"
	}
	if realRoot, err = onDisk(realRoot); err != nil {
		return Path{}, "the chat folder is not readable"
	}
	rel, err := filepath.Rel(realRoot, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Path{}, a.Path + ": outside the chat folder"
	}
	if t.Denied(full) {
		return Path{}, a.Path + ": not allowed (credential file)"
	}
	return Path{Root: realRoot, Full: full, Rel: rel}, ""
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

func (t Guard) Denied(full string) bool {
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

func (t Guard) list(ctx context.Context, raw []byte) (task.ToolResult, error) {
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
			if t.Denied(filepath.Join(target.Full, name)) {
				continue
			}
			name += "/"
		} else if t.Denied(filepath.Join(target.Full, name)) {
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
