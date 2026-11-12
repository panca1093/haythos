package gitread

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/adapters/tools/local/readfile"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

type fixture struct {
	reg  *local.Registry
	root string
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "project")
	write(t, root, "main.go", "package main\n")
	write(t, root, "notes.md", "one\n")
	write(t, root, ".env", "TOKEN=secret-one\n")
	write(t, root, "certs/x.pem", "PEM-ONE\n")
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "first commit")
	write(t, root, "main.go", "package main\n\nfunc main() {}\n")
	write(t, root, ".env", "TOKEN=secret-two\n")
	write(t, root, "certs/x.pem", "PEM-TWO\n")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "second commit")
	write(t, root, "notes.md", "one\ntwo\n")
	gitIn(t, root, "add", "notes.md")
	write(t, root, "main.go", "package main\n\nfunc main() { println(1) }\n")
	write(t, root, ".env", "TOKEN=secret-three\n")
	write(t, root, "certs/x.pem", "PEM-THREE\n")
	reg := local.New()
	Register(reg, readfile.Config{Home: home})
	return fixture{reg: reg, root: root}
}

func (f fixture) call(t *testing.T, tool, args string) task.ToolResult {
	t.Helper()
	res, err := f.reg.Call(app.WithWorkdir(context.Background(), f.root), task.ToolCall{ID: "c", Name: tool, Args: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("%s %s: %v", tool, args, err)
	}
	return res
}

func TestSpecsAreReadOnly(t *testing.T) {
	f := newFixture(t)
	specs := f.reg.List([]string{"git.status", "git.diff", "git.log", "git.show"})
	if len(specs) != 4 {
		t.Fatalf("specs = %+v", specs)
	}
	for _, s := range specs {
		if !json.Valid(s.InputSchema) {
			t.Errorf("%s: invalid schema %s", s.Name, s.InputSchema)
		}
		if s.Write || s.Untrusted {
			t.Errorf("%s: write=%v untrusted=%v", s.Name, s.Write, s.Untrusted)
		}
	}
}

func TestBasicOutputAndTrust(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, tool, args string
		want             []string
		untrusted        bool
	}{
		{"status", "git.status", `{}`, []string{"## ", "main.go", "notes.md"}, false},
		{"diff", "git.diff", `{"path":"main.go"}`, []string{"println(1)"}, true},
		{"diff staged", "git.diff", `{"staged":true}`, []string{"+two"}, true},
		{"diff stat", "git.diff", `{"stat":true}`, []string{"main.go"}, false},
		{"diff ref", "git.diff", `{"ref":"HEAD~1","path":"main.go"}`, []string{"func main()"}, true},
		{"log", "git.log", `{}`, []string{"second commit", "first commit"}, true},
		{"log limit", "git.log", `{"limit":1}`, []string{"second commit"}, true},
		{"log path", "git.log", `{"path":"notes.md"}`, []string{"first commit"}, true},
		{"show", "git.show", `{"ref":"HEAD","path":"main.go"}`, []string{"second commit", "+func main() {}"}, true},
		{"show stat", "git.show", `{"ref":"HEAD","stat":true}`, []string{"main.go"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := f.call(t, tc.tool, tc.args)
			if res.IsError {
				t.Fatalf("error: %s", res.Content)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.Content, w) {
					t.Errorf("missing %q in:\n%s", w, res.Content)
				}
			}
			if res.Untrusted != tc.untrusted {
				t.Errorf("untrusted = %v, want %v", res.Untrusted, tc.untrusted)
			}
		})
	}
	if res := f.call(t, "git.log", `{"limit":1}`); strings.Contains(res.Content, "first commit") {
		t.Errorf("limit ignored: %s", res.Content)
	}
}

func TestShowStatKeepsCommitMessageOut(t *testing.T) {
	f := newFixture(t)
	if res := f.call(t, "git.show", `{"ref":"HEAD","stat":true}`); strings.Contains(res.Content, "second commit") {
		t.Errorf("trusted stat output carries commit text: %s", res.Content)
	}
}

func TestRefusedArguments(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(f.root, "pwned")
	tests := []struct{ name, tool, args string }{
		{"diff ref dash", "git.diff", `{"ref":"--output=` + out + `"}`},
		{"diff path dash", "git.diff", `{"path":"--output=` + out + `"}`},
		{"log ref dash", "git.log", `{"ref":"-p"}`},
		{"log path dash", "git.log", `{"path":"-p"}`},
		{"show ref dash", "git.show", `{"ref":"--output=` + out + `"}`},
		{"show path dash", "git.show", `{"ref":"HEAD","path":"--x"}`},
		{"show empty ref", "git.show", `{"ref":"  "}`},
		{"show missing ref", "git.show", `{}`},
		{"blob ref", "git.show", `{"ref":"HEAD:.env"}`},
		{"diff blob ref", "git.diff", `{"ref":"HEAD:.env"}`},
		{"outside folder", "git.diff", `{"path":"../x"}`},
		{"absolute path", "git.log", `{"path":"/etc/passwd"}`},
		{"denied path", "git.diff", `{"path":".env"}`},
		{"denied dir path", "git.log", `{"path":"certs/x.pem"}`},
		{"bad json", "git.log", `{"limit":"x"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if res := f.call(t, tc.tool, tc.args); !res.IsError {
				t.Fatalf("want refusal, got %q", res.Content)
			}
		})
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("--output wrote a file")
	}
}

func TestRepoConfigProgramsDoNotRun(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(t.TempDir(), "marker")
	script := filepath.Join(t.TempDir(), "evil.sh")
	write(t, filepath.Dir(script), filepath.Base(script), "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(script, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"core.fsmonitor", script}, {"diff.external", script}, {"core.pager", script}, {"core.hooksPath", filepath.Dir(script)}} {
		gitIn(t, f.root, "config", kv[0], kv[1])
	}
	write(t, f.root, ".gitattributes", "*.go diff=evil filter=evil\n")
	gitIn(t, f.root, "config", "diff.evil.textconv", script)
	for _, k := range []string{"filter.evil.clean", "filter.evil.smudge", "filter.evil.process", "gpg.program"} {
		gitIn(t, f.root, "config", k, script)
	}
	gitIn(t, f.root, "config", "log.showSignature", "true")
	signHead(t, f.root)
	for _, c := range []struct{ tool, args string }{
		{"git.status", `{}`}, {"git.diff", `{}`}, {"git.diff", `{"staged":true}`}, {"git.show", `{"ref":"HEAD"}`},
		{"git.show", `{"ref":"HEAD","stat":true}`}, {"git.log", `{}`},
	} {
		if res := f.call(t, c.tool, c.args); res.IsError {
			t.Fatalf("%s: %s", c.tool, res.Content)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a program from .git/config ran")
	}
}

func TestDeniedPathsAreDropped(t *testing.T) {
	f := newFixture(t)
	gitIn(t, f.root, "add", "-A")
	gitIn(t, f.root, "commit", "-q", "-m", "third commit")
	write(t, f.root, ".env", "TOKEN=secret-four\n")
	write(t, f.root, "main.go", "package main\n// edited\n")
	for _, c := range []struct{ tool, args string }{
		{"git.status", `{}`}, {"git.diff", `{}`}, {"git.diff", `{"stat":true}`}, {"git.diff", `{"ref":"HEAD~1"}`},
		{"git.show", `{"ref":"HEAD~1"}`}, {"git.show", `{"ref":"HEAD~1","stat":true}`}, {"git.show", `{"ref":"HEAD~2"}`},
	} {
		t.Run(c.tool+c.args, func(t *testing.T) {
			res := f.call(t, c.tool, c.args)
			if res.IsError {
				t.Fatalf("error: %s", res.Content)
			}
			for _, bad := range []string{".env", "x.pem", "secret", "PEM-"} {
				if strings.Contains(res.Content, bad) {
					t.Errorf("%q leaked:\n%s", bad, res.Content)
				}
			}
		})
	}
	if res := f.call(t, "git.diff", `{}`); !strings.Contains(res.Content, "// edited") {
		t.Errorf("allowed file section lost:\n%s", res.Content)
	}
	if res := f.call(t, "git.status", `{}`); !strings.Contains(res.Content, "main.go") {
		t.Errorf("allowed file lost:\n%s", res.Content)
	}
}

func TestOutputIsCapped(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, "big.txt", strings.Repeat("line of text\n", 5000))
	gitIn(t, f.root, "add", "big.txt")
	res := f.call(t, "git.diff", `{"staged":true}`)
	if res.IsError {
		t.Fatal(res.Content)
	}
	if !strings.HasSuffix(res.Content, "[truncated; narrow path or ref]") {
		t.Errorf("no marker at the end: %q", res.Content[max(0, len(res.Content)-80):])
	}
	if len(res.Content) > maxOut+100 {
		t.Errorf("len = %d, want about %d", len(res.Content), maxOut)
	}
}

func TestNotARepository(t *testing.T) {
	f := newFixture(t)
	f.root = t.TempDir()
	res := f.call(t, "git.status", `{}`)
	if !res.IsError || !strings.Contains(res.Content, "not a git repository") {
		t.Fatalf("result = %+v", res)
	}
}

func TestScopedToChatFolder(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, "pkg/a.go", "package a\n")
	gitIn(t, f.root, "add", "pkg")
	f.root = filepath.Join(f.root, "pkg")
	for _, tool := range []string{"git.status", "git.diff"} {
		res := f.call(t, tool, `{"staged":true}`)
		if res.IsError || !strings.Contains(res.Content, "a.go") || strings.Contains(res.Content, "notes.md") || strings.Contains(res.Content, "main.go") {
			t.Errorf("%s = %+v, want only the chat folder's changes", tool, res)
		}
	}
}

// signHead rewrites HEAD with a fake gpgsig header, enough for git to call gpg.program.
func signHead(t *testing.T, root string) {
	t.Helper()
	raw, err := exec.CommandContext(t.Context(), "git", "-C", root, "cat-file", "commit", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head, body, _ := strings.Cut(string(raw), "\n\n")
	signed := head + "\ngpgsig -----BEGIN PGP SIGNATURE-----\n x\n -----END PGP SIGNATURE-----\n\n" + body
	cmd := exec.CommandContext(t.Context(), "git", "-C", root, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(signed)
	id, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "update-ref", "HEAD", strings.TrimSpace(string(id)))
}

func TestShowStatOfATagIsNotTrustedText(t *testing.T) {
	f := newFixture(t)
	gitIn(t, f.root, "tag", "-a", "v1", "-m", "tag message attacker text")
	blob, err := exec.CommandContext(t.Context(), "git", "-C", f.root, "rev-parse", "HEAD:notes.md").Output()
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.root, "tag", "blobtag", strings.TrimSpace(string(blob)))
	for _, ref := range []string{"v1", "blobtag"} {
		res := f.call(t, "git.show", `{"ref":"`+ref+`","stat":true}`)
		if !res.IsError && !res.Untrusted && (strings.Contains(res.Content, "attacker") || strings.Contains(res.Content, "one")) {
			t.Fatalf("%s: trusted stat output carries text written by others: %q", ref, res.Content)
		}
	}
}
