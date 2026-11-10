package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

type fixture struct {
	reg        *local.Registry
	home, root string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "project")
	files := map[string]string{
		"project/README.md":        "# hello",
		"project/src/main.go":      "package main",
		"project/.env":             "TOKEN=secret",
		"project/.env.local":       "TOKEN=secret",
		"project/certs/server.pem": "-----BEGIN",
		"project/keys/id_ed25519":  "key",
		"project/vault.enc":        "cipher",
		"project/bin.dat":          "\xff\xfe\x00",
		"outside.txt":              "nope",
		".ssh/config":              "Host x",
	}
	for p, body := range files {
		full := filepath.Join(home, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "outside.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	reg := local.New()
	Register(reg, Config{Home: home, Deny: []string{filepath.Join(root, "vault.enc")}})
	return fixture{reg: reg, home: home, root: root}
}

func (f fixture) call(t *testing.T, workdir, tool, args string) task.ToolResult {
	t.Helper()
	res, err := f.reg.Call(app.WithWorkdir(context.Background(), workdir), task.ToolCall{ID: "c", Name: tool, Args: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("%s %s: %v", tool, args, err)
	}
	return res
}

func TestSpecsAreReadOnlyAndUntrusted(t *testing.T) {
	f := newFixture(t)
	specs := f.reg.List([]string{"fs.list", "fs.read"})
	if len(specs) != 2 {
		t.Fatalf("specs = %+v", specs)
	}
	for _, s := range specs {
		if s.Write || !s.Untrusted {
			t.Errorf("%s: write=%v untrusted=%v, want read-only and untrusted", s.Name, s.Write, s.Untrusted)
		}
	}
}

func TestRead(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name, workdir, path string
		wantErr             bool
		want                string
	}{
		{"file in folder", f.root, "README.md", false, "# hello"},
		{"nested", f.root, "src/main.go", false, "package main"},
		{"no folder for this chat", "", "README.md", true, "no folder"},
		{"parent escape", f.root, "../outside.txt", true, "outside"},
		{"absolute path", f.root, filepath.Join(f.home, "outside.txt"), true, "relative"},
		{"symlink escape", f.root, "escape.txt", true, "outside"},
		{"dotenv", f.root, ".env", true, "not allowed"},
		{"dotenv variant", f.root, ".env.local", true, "not allowed"},
		{"pem", f.root, "certs/server.pem", true, "not allowed"},
		{"ssh key name", f.root, "keys/id_ed25519", true, "not allowed"},
		{"configured deny", f.root, "vault.enc", true, "not allowed"},
		{"binary", f.root, "bin.dat", true, "not text"},
		{"missing", f.root, "nope.md", true, "not found"},
		{"ssh dir when chat opened in home", f.home, ".ssh/config", true, "not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]string{"path": tc.path})
			res := f.call(t, tc.workdir, "fs.read", string(b))
			if res.IsError != tc.wantErr || !strings.Contains(res.Content, tc.want) {
				t.Fatalf("result = %+v, want error=%v containing %q", res, tc.wantErr, tc.want)
			}
			if strings.Contains(res.Content, "secret") || strings.Contains(res.Content, "Host x") {
				t.Fatalf("leaked a denied file: %q", res.Content)
			}
		})
	}
}

func TestReadTruncatesLargeFiles(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "big.txt"), []byte(strings.Repeat("a", maxRead+100)), 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, f.root, "fs.read", `{"path":"big.txt"}`)
	if res.IsError || len(res.Content) > maxRead+200 || !strings.Contains(res.Content, "truncated") {
		t.Fatalf("len=%d error=%v, want at most %d bytes plus a truncation note", len(res.Content), res.IsError, maxRead)
	}
}

func TestList(t *testing.T) {
	f := newFixture(t)
	res := f.call(t, f.root, "fs.list", `{}`)
	if res.IsError {
		t.Fatalf("list: %+v", res)
	}
	for _, want := range []string{"README.md", "src/", "certs/"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("listing lacks %q:\n%s", want, res.Content)
		}
	}
	for _, hidden := range []string{".env", "id_ed25519", "vault.enc"} {
		if strings.Contains(res.Content, hidden) {
			t.Errorf("listing shows denied %q:\n%s", hidden, res.Content)
		}
	}
	if res := f.call(t, f.root, "fs.list", `{"path":".."}`); !res.IsError {
		t.Errorf("listing the parent = %+v, want refused", res)
	}
	if res := f.call(t, f.home, "fs.list", `{"path":".ssh"}`); !res.IsError {
		t.Errorf("listing .ssh = %+v, want refused", res)
	}
}
