package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestSpecsAreReadOnlyAndLeaveTrustToTheResult(t *testing.T) {
	f := newFixture(t)
	specs := f.reg.List([]string{"fs.list", "fs.read", "fs.glob", "fs.grep"})
	if len(specs) != 4 {
		t.Fatalf("specs = %+v", specs)
	}
	for _, s := range specs {
		if !json.Valid(s.InputSchema) {
			t.Errorf("%s has invalid JSON schema: %s", s.Name, s.InputSchema)
		}
		if s.Write || s.Untrusted {
			t.Errorf("%s: write=%v untrusted=%v, want read-only with trust decided per result", s.Name, s.Write, s.Untrusted)
		}
	}
}

func TestReadMarksOnlyNonCodeAsUntrusted(t *testing.T) {
	f := newFixture(t)
	files := map[string]string{
		"a.go": "package a", "B.PY": "x = 1", "c.ts": "export {}", "d.sh": "echo", "q.sql": "select 1",
		"e.md": "# e", "f.txt": "t", "g.json": "{}", "h.yaml": "a: 1", "Makefile": "all:", "noext": "x", "notes.go.md": "x", "page.html": "<p>",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("e.md", filepath.Join(f.root, "link.go")); err != nil {
		t.Fatal(err)
	}
	for path, untrusted := range map[string]bool{
		"a.go": false, "B.PY": false, "c.ts": false, "d.sh": false, "q.sql": false, "src/main.go": false,
		"e.md": true, "f.txt": true, "g.json": true, "h.yaml": true, "Makefile": true, "noext": true, "notes.go.md": true, "page.html": true,
		"link.go": true,
	} {
		t.Run(path, func(t *testing.T) {
			b, _ := json.Marshal(map[string]string{"path": path})
			res := f.call(t, f.root, "fs.read", string(b))
			if res.IsError || res.Untrusted != untrusted {
				t.Fatalf("result = %+v, want untrusted=%v: only source code is a trusted source, and a link is judged by what it points to", res, untrusted)
			}
		})
	}
}

func TestReadAndListHideInvisibleCharacters(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "x.go"), []byte("package main // \u202eevil\u200b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "y\u200bz.go"), []byte("package y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := f.call(t, f.root, "fs.read", `{"path":"x.go"}`); res.Content != "package main // evil" {
		t.Errorf("read = %q, want the invisible characters removed from trusted code", res.Content)
	}
	res := f.call(t, f.root, "fs.list", `{}`)
	if res.Untrusted || strings.ContainsAny(res.Content, "\u200b\u202e") || !strings.Contains(res.Content, "yz.go") {
		t.Errorf("list = %+v, want a trusted listing without invisible characters", res)
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

func TestReadRangeReturnsOnlyRequestedLines(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "lines.go"), []byte("first\nsecond\nthird\nfourth\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, f.root, "fs.read", `{"path":"lines.go","start_line":2,"end_line":3}`)
	if res.IsError || res.Content != "second\nthird" {
		t.Fatalf("result = %+v, want lines 2 through 3", res)
	}
}

func TestSearchIsScopedAndReturnsPathsOrMatchingLines(t *testing.T) {
	f := newFixture(t)
	glob := f.call(t, f.root, "fs.glob", `{"pattern":"*.go"}`)
	if glob.IsError || glob.Content != "src/main.go" {
		t.Fatalf("glob = %+v", glob)
	}
	grep := f.call(t, f.root, "fs.grep", `{"query":"package","pattern":"*.go"}`)
	if grep.IsError || grep.Content != "src/main.go:1:package main" || grep.Untrusted {
		t.Fatalf("grep = %+v", grep)
	}
	hidden := f.call(t, f.root, "fs.grep", `{"query":"secret"}`)
	if hidden.IsError || strings.Contains(hidden.Content, "TOKEN") {
		t.Fatalf("grep returned a denied file: %+v", hidden)
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

func TestDeniedIsCaseInsensitiveAndCoversMoreCredentials(t *testing.T) {
	home := t.TempDir()
	real, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	tl := tool{cfg: Config{Home: home, Deny: []string{filepath.Join(home, "Vault.enc")}}}
	for _, tc := range []struct {
		rel  string
		want bool
	}{
		{".SSH/ID_RSA", true},
		{"proj/.ENV", true},
		{"proj/.Env.Local", true},
		{".CONFIG/GH/hosts.yml", true},
		{".config/gh/hosts.yml", true},
		{".config/ghostty/config", false},
		{".config/gcloud/credentials.db", true},
		{".config/GCLOUD", true},
		{"proj/.envrc", true},
		{"proj/prod.env", true},
		{"proj/.pgpass", true},
		{"proj/.my.cnf", true},
		{"proj/terraform.tfstate", true},
		{"proj/.zsh_history", true},
		{"proj/.bash_history", true},
		{"proj/.python_history", true},
		{"proj/.node_repl_history", true},
		{"proj/.psql_history", true},
		{"proj/.git-credentials", true},
		{"proj/.git/config", true},
		{"proj/.GIT/CONFIG", true},
		{"proj/.git/HEAD", false},
		{"proj/config", false},
		{".azure/accessTokens.json", true},
		{".terraform.d/credentials.tfrc.json", true},
		{".claude/settings.json", true},
		{".codex/auth.json", true},
		{"proj/README.md", false},
		{"proj/environment.go", false},
	} {
		t.Run(tc.rel, func(t *testing.T) {
			if got := tl.denied(filepath.Join(real, filepath.FromSlash(tc.rel))); got != tc.want {
				t.Fatalf("denied(%q) = %v, want %v", tc.rel, got, tc.want)
			}
		})
	}
}

func TestReadRefusesNonRegularFiles(t *testing.T) {
	f := newFixture(t)
	fifo := filepath.Join(f.root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan task.ToolResult, 1)
	go func() { done <- f.call(t, f.root, "fs.read", `{"path":"pipe"}`) }()
	select {
	case res := <-done:
		if !res.IsError {
			t.Fatalf("fifo read = %+v, want refused", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fs.read blocked on a fifo")
	}
}

func TestReadTruncationKeepsTextAfterInvalidByte(t *testing.T) {
	f := newFixture(t)
	body := "caf\xff" + strings.Repeat("a", maxRead) + "é"
	if err := os.WriteFile(filepath.Join(f.root, "mixed.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := f.call(t, f.root, "fs.read", `{"path":"mixed.txt"}`); !res.IsError || !strings.Contains(res.Content, "not text") {
		t.Fatalf("result = %+v, want binary refusal", res)
	}
	split := strings.Repeat("a", maxRead-1) + "é"
	if err := os.WriteFile(filepath.Join(f.root, "split.txt"), []byte(split), 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, f.root, "fs.read", `{"path":"split.txt"}`)
	if res.IsError || !strings.Contains(res.Content, "truncated") {
		t.Fatalf("result error=%v, want a truncated text read", res.IsError)
	}
}

func TestCaseAndFoldVariantsOfDeniedPathsAreRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.home, ".bash_history"), []byte("secret cmd"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".SSH/config", ".\u017fsh/config", ".bash_hi\ufb06ory", "project/.ENV", "project/.env.LOCAL"} {
		t.Run(p, func(t *testing.T) {
			b, _ := json.Marshal(map[string]string{"path": p})
			res := f.call(t, f.home, "fs.read", string(b))
			if !res.IsError || strings.Contains(res.Content, "Host x") || strings.Contains(res.Content, "secret") {
				t.Fatalf("read %q = %+v, want refused without content", p, res)
			}
		})
	}
}
