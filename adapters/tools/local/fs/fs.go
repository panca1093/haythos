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
	deniedDirs  = []string{".ssh", ".gnupg", ".aws", ".haythos", ".kube", ".docker"}
	deniedNames = []string{".netrc", ".npmrc", ".pypirc", ".git-credentials"}
	deniedExts  = []string{".pem", ".key", ".p12", ".pfx", ".enc"}
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
	rel, err := filepath.Rel(realRoot, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", a.Path + ": outside the chat folder"
	}
	if t.denied(full) {
		return "", a.Path + ": not allowed (credential file)"
	}
	return full, ""
}

func (t tool) denied(full string) bool {
	for _, d := range t.cfg.Deny {
		if real, err := filepath.EvalSymlinks(d); err == nil && real == full {
			return true
		}
	}
	if t.cfg.Home != "" {
		if home, err := filepath.EvalSymlinks(t.cfg.Home); err == nil && strings.HasPrefix(full, filepath.Join(home, ".config", "gh")) {
			return true
		}
	}
	for _, part := range strings.Split(full, string(filepath.Separator)) {
		for _, d := range deniedDirs {
			if part == d {
				return true
			}
		}
	}
	return deniedName(filepath.Base(full))
}

func deniedName(name string) bool {
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
	f, err := os.Open(full) //nolint:gosec // path is confined to the chat folder by resolve
	if err != nil {
		return refuse("not readable")
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxRead+1))
	if err != nil {
		return refuse("not readable")
	}
	truncated := len(b) > maxRead
	if truncated {
		b = b[:maxRead]
		for len(b) > 0 && !utf8.Valid(b) {
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
