// Package fs gives Alfred read-only access to the folder a chat was opened in.
package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	maxRead = 64 << 10
	maxList = 200
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
)

type tool struct{ cfg Config }

func Register(r *local.Registry, cfg Config) {
	t := tool{cfg: cfg}
	pathArg := `{"type":"object","properties":{"path":{"type":"string","description":"path relative to the chat folder"}}`
	r.Register(task.ToolSpec{
		Name:        "fs.list",
		Description: "List files and folders in the folder this chat was opened in (or a subfolder). Read-only.",
		InputSchema: json.RawMessage(pathArg + `}`),
		Untrusted:   true,
	}, t.list)
	r.Register(task.ToolSpec{
		Name:        "fs.read",
		Description: "Read a text file inside the folder this chat was opened in. Read-only; large files are truncated.",
		InputSchema: json.RawMessage(pathArg + `,"required":["path"]}`),
		Untrusted:   true,
	}, t.read)
}

func refuse(msg string) (task.ToolResult, error) {
	return task.ToolResult{Content: msg, IsError: true}, nil
}

type args struct {
	Path string `json:"path"`
}

// resolve returns the real path of rel inside the chat folder, or a reason the model may read.
func (t tool) resolve(ctx context.Context, raw []byte) (string, string) {
	root := app.WorkdirFrom(ctx)
	if root == "" {
		return "", "this chat has no folder; file tools only work in `haythos chat`"
	}
	var a args
	if len(raw) > 0 && json.Unmarshal(raw, &a) != nil {
		return "", "arguments must be {\"path\": \"...\"}"
	}
	if a.Path == "" {
		a.Path = "."
	}
	if filepath.IsAbs(a.Path) {
		return "", "use a path relative to the chat folder"
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "the chat folder is not readable"
	}
	full, err := filepath.EvalSymlinks(filepath.Join(realRoot, a.Path))
	if errors.Is(err, os.ErrNotExist) {
		return "", a.Path + ": not found"
	}
	if err != nil {
		return "", a.Path + ": not readable"
	}
	if full, err = onDisk(full); err != nil {
		return "", a.Path + ": not readable"
	}
	if realRoot, err = onDisk(realRoot); err != nil {
		return "", "the chat folder is not readable"
	}
	rel, err := filepath.Rel(realRoot, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", a.Path + ": outside the chat folder"
	}
	if t.denied(full) {
		return "", a.Path + ": not allowed (credential file)"
	}
	return full, ""
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
	full, why := t.resolve(ctx, raw)
	if why != "" {
		return refuse(why)
	}
	resolved, err := os.Lstat(full)
	if err != nil || !resolved.Mode().IsRegular() {
		return refuse("not a regular file")
	}
	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // path is confined to the chat folder by resolve
	if err != nil {
		return refuse("not readable")
	}
	defer func() { _ = f.Close() }()
	if opened, err := f.Stat(); err != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, resolved) {
		return refuse("not readable")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxRead+1))
	if err != nil {
		return refuse("not readable")
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
		out += fmt.Sprintf("\n[truncated at %d KiB]", maxRead>>10)
	}
	return task.ToolResult{Content: out}, nil
}

func (t tool) list(ctx context.Context, raw []byte) (task.ToolResult, error) {
	full, why := t.resolve(ctx, raw)
	if why != "" {
		return refuse(why)
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return refuse("not a readable folder")
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if t.denied(filepath.Join(full, name)) {
				continue
			}
			name += "/"
		} else if t.denied(filepath.Join(full, name)) {
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
	return task.ToolResult{Content: strings.Join(names, "\n") + more}, nil
}
