package readfile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

func searchFixture(t *testing.T) fixture {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	f := newFixture(t)
	files := map[string]string{
		"src/a.go":       "package a\nfunc Needle() {}\n// needle again\n",
		"src/b.go":       "package b\nfunc Other() {}\n",
		"docs/notes.txt": "a needle in prose\n",
		".hidden/h.txt":  "needle hidden\n",
		"many/m1.txt":    "needle\n",
		"many/m2.txt":    "needle\n",
		"many/m3.txt":    "needle\n",
	}
	for p, body := range files {
		full := filepath.Join(f.root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.root, "certs/note.txt"), []byte("BEGIN note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func lines(s string) []string { return strings.Split(s, "\n") }

func TestSearchModes(t *testing.T) {
	f := searchFixture(t)
	tests := []struct {
		name, args string
		want       []string
		untrusted  bool
	}{
		{"files is the default", `{"pattern":"needle","path":"src"}`, []string{"src/a.go"}, false},
		{"files across tree skips hidden", `{"pattern":"needle"}`, []string{"docs/notes.txt", "many/m1.txt", "many/m2.txt", "many/m3.txt", "src/a.go"}, false},
		{"content in source is trusted", `{"pattern":"needle","path":"src","mode":"content"}`, []string{"src/a.go:3:// needle again"}, false},
		{"content in prose is untrusted", `{"pattern":"needle","path":"docs","mode":"content"}`, []string{"docs/notes.txt:1:a needle in prose"}, true},
		{"count", `{"pattern":"(?i)needle","path":"src","mode":"count"}`, []string{"src/a.go:2"}, false},
		{"glob", `{"pattern":"needle","glob":"*.txt","path":"docs"}`, []string{"docs/notes.txt"}, false},
		{"single file", `{"pattern":"Needle","path":"src/a.go","mode":"content"}`, []string{"src/a.go:2:func Needle() {}"}, false},
		{"no match", `{"pattern":"zzzz"}`, []string{"No matches."}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := f.call(t, f.root, "file.search", tc.args)
			if res.IsError {
				t.Fatalf("error: %s", res.Content)
			}
			if got := lines(res.Content); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if res.Untrusted != tc.untrusted {
				t.Errorf("untrusted = %v, want %v", res.Untrusted, tc.untrusted)
			}
		})
	}
}

func TestSearchLimit(t *testing.T) {
	f := searchFixture(t)
	res := f.call(t, f.root, "file.search", `{"pattern":"needle","path":"many","limit":2}`)
	want := "many/m1.txt\nmany/m2.txt\n" + moreResults
	if res.Content != want {
		t.Errorf("got %q, want %q", res.Content, want)
	}
	res = f.call(t, f.root, "file.search", `{"pattern":"needle","path":"many","limit":3}`)
	if strings.Contains(res.Content, moreResults) {
		t.Errorf("marker at exact limit: %q", res.Content)
	}
}

func TestSearchOutputCap(t *testing.T) {
	f := searchFixture(t)
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "needle %s\n", strings.Repeat("x", 100))
	}
	if err := os.WriteFile(filepath.Join(f.root, "big.txt"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, f.root, "file.search", `{"pattern":"needle","path":"big.txt","mode":"content","limit":400}`)
	if len(res.Content) > maxSearchOutput+len(moreResults)+2 || !strings.HasSuffix(res.Content, moreResults) {
		t.Errorf("len = %d, tail = %q", len(res.Content), res.Content[max(0, len(res.Content)-60):])
	}
}

func TestSearchNeverReturnsDeniedFiles(t *testing.T) {
	f := searchFixture(t)
	for _, args := range []string{
		`{"pattern":"TOKEN"}`,
		`{"pattern":"BEGIN"}`,
		`{"pattern":"BEGIN","mode":"content"}`,
		`{"pattern":"BEGIN","mode":"count"}`,
		`{"pattern":"cipher|key|secret"}`,
	} {
		res := f.call(t, f.root, "file.search", args)
		for _, bad := range []string{".env", "server.pem", "id_ed25519", "vault.enc"} {
			if strings.Contains(res.Content, bad) {
				t.Errorf("%s leaked %s: %q", args, bad, res.Content)
			}
		}
	}
	res := f.call(t, f.root, "file.search", `{"pattern":"BEGIN"}`)
	if res.Content != "certs/note.txt" {
		t.Errorf("allowed sibling missing: %q", res.Content)
	}
	if res := f.call(t, f.root, "file.search", `{"pattern":"x","path":"certs/server.pem"}`); !res.IsError {
		t.Errorf("denied path searched: %q", res.Content)
	}
}

func TestSearchRefusals(t *testing.T) {
	f := searchFixture(t)
	for name, args := range map[string]string{
		"no pattern":    `{}`,
		"bad mode":      `{"pattern":"x","mode":"all"}`,
		"negative":      `{"pattern":"x","limit":-1}`,
		"outside":       `{"pattern":"x","path":"../"}`,
		"absolute":      `{"pattern":"x","path":"/etc"}`,
		"bad regex":     `{"pattern":"("}`,
		"missing path":  `{"pattern":"x","path":"nope"}`,
		"malformed arg": `{"pattern":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			if res := f.call(t, f.root, "file.search", args); !res.IsError {
				t.Errorf("got %q, want error", res.Content)
			}
		})
	}
	if res := f.call(t, "", "file.search", `{"pattern":"x"}`); !res.IsError {
		t.Error("no chat folder should be refused")
	}
}

func TestSearchMissingRg(t *testing.T) {
	f := newFixture(t)
	reg := local.New()
	Register(reg, Config{Home: f.home, Rg: filepath.Join(t.TempDir(), "no-rg")})
	res, err := reg.Call(app.WithWorkdir(context.Background(), f.root), task.ToolCall{ID: "c", Name: "file.search", Args: json.RawMessage(`{"pattern":"x"}`)})
	if err != nil || !res.IsError || res.Content != "file.search needs ripgrep (rg) installed" {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

func TestSearchTimeout(t *testing.T) {
	f := newFixture(t)
	slow := filepath.Join(t.TempDir(), "rg")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	reg := local.New()
	Register(reg, Config{Home: f.home, Rg: slow})
	ctx, cancel := context.WithTimeout(app.WithWorkdir(context.Background(), f.root), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := reg.Call(ctx, task.ToolCall{ID: "c", Name: "file.search", Args: json.RawMessage(`{"pattern":"x"}`)})
	if err != nil || !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Errorf("res = %+v, err = %v", res, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}
