//go:build integration

package spike

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var guardAsk = []string{"Bash(rm *)", "Bash(git reset --hard*)", "Bash(ssh *)", "Bash(sudo *)", "Bash(git push --force*)", "Bash(git push -f*)", "Read(./.env)", "Read(~/.ssh/**)"}

func claudeSettings(allow []string) string {
	b, _ := json.Marshal(frame{"permissions": frame{"allow": allow, "ask": guardAsk}})
	return string(b)
}

func claudeStart(t *testing.T, dir string, args ...string) *proc {
	t.Helper()
	base := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--permission-prompt-tool", "stdio", "--model", "haiku", "--setting-sources", "", "--no-session-persistence"}
	p := start(t, dir, childEnv(), "claude", append(base, args...)...)
	p.send(frame{"type": "control_request", "request_id": "init-1", "request": frame{"subtype": "initialize"}})
	return p
}

// claudeTurn sends a user message and answers can_use_tool with allow or deny until the result frame.
// A non-empty interruptOn interrupts the turn when a frame of that type arrives.
func claudeTurn(t *testing.T, p *proc, prompt string, allow bool, interruptOn string) (result frame, asked []string) {
	t.Helper()
	p.send(frame{"type": "user", "message": frame{"role": "user", "content": []any{frame{"type": "text", "text": prompt}}}})
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		f, ok := p.recv(60 * time.Second)
		if !ok {
			t.Fatal("no frame / eof")
		}
		switch str(f, "type") {
		case "control_request":
			req := sub(f, "request")
			if str(req, "subtype") != "can_use_tool" {
				continue
			}
			asked = append(asked, str(req, "tool_name")+" "+trim(str(req, "input"), 120))
			var resp frame
			if allow {
				resp = frame{"behavior": "allow", "updatedInput": req["input"]}
			} else {
				resp = frame{"behavior": "deny", "message": "denied by spike"}
			}
			p.send(frame{"type": "control_response", "response": frame{"subtype": "success", "request_id": f["request_id"], "response": resp}})
		case "result":
			return f, asked
		default:
			if interruptOn != "" && str(f, "type") == interruptOn {
				interruptOn = ""
				p.send(frame{"type": "control_request", "request_id": "int-1", "request": frame{"subtype": "interrupt"}})
			}
		}
	}
	t.Fatal("timeout")
	return nil, nil
}

func logResult(t *testing.T, r frame) {
	t.Logf("result: subtype=%s is_error=%v stop_reason=%v terminal_reason=%v num_turns=%v result=%q", str(r, "subtype"), r["is_error"], r["stop_reason"], r["terminal_reason"], r["num_turns"], trim(str(r, "result"), 120))
}

func TestClaudeStreamJSON(t *testing.T) {
	t.Run("ask-rules-in-default-mode", func(t *testing.T) {
		dir := scratchRepo(t)
		_ = os.WriteFile(filepath.Join(dir, ".env"), []byte("FAKE_TOKEN=not-a-secret\n"), 0o600)
		p := claudeStart(t, dir, "--settings", claudeSettings([]string{"Bash", "Read", "Edit", "Write"}))
		r, asked := claudeTurn(t, p, "Append the line 'hello from claude' to README.md. Reply 'done' only.", true, "")
		logResult(t, r)
		t.Logf("asked in turn 1 (want none): %v; README=%q", asked, readFile(dir, "README.md"))
		r, asked = claudeTurn(t, p, "Run `rm victim.txt` with the Bash tool. Reply 'done' only.", false, "")
		logResult(t, r)
		t.Logf("asked rm: %v; victim=%q", asked, readFile(dir, "victim.txt"))
		r, asked = claudeTurn(t, p, "Use the Read tool on .env and tell me its first word.", false, "")
		logResult(t, r)
		t.Logf("asked Read .env: %v", asked)
		r, asked = claudeTurn(t, p, "Run `cat .env` with the Bash tool and tell me its first word.", false, "")
		logResult(t, r)
		t.Logf("asked cat .env (Bash): %v", asked)
	})
	t.Run("ask-rules-in-bypassPermissions", func(t *testing.T) {
		dir := scratchRepo(t)
		p := claudeStart(t, dir, "--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions", "--settings", claudeSettings(nil))
		r, asked := claudeTurn(t, p, "Run `rm victim.txt` with the Bash tool. Reply 'done' only.", false, "")
		logResult(t, r)
		t.Logf("asked rm: %v; victim=%q", asked, readFile(dir, "victim.txt"))
	})
	t.Run("ask-rules-in-acceptEdits", func(t *testing.T) {
		dir := scratchRepo(t)
		p := claudeStart(t, dir, "--permission-mode", "acceptEdits", "--settings", claudeSettings([]string{"Bash"}))
		r, asked := claudeTurn(t, p, "Run `rm victim.txt` with the Bash tool. Reply 'done' only.", false, "")
		logResult(t, r)
		t.Logf("asked rm: %v; victim=%q", asked, readFile(dir, "victim.txt"))
	})
	t.Run("interrupt", func(t *testing.T) {
		dir := scratchRepo(t)
		p := claudeStart(t, dir, "--settings", claudeSettings([]string{"Bash"}))
		r, _ := claudeTurn(t, p, "Run `sleep 60` with the Bash tool, then reply 'done'.", true, "assistant")
		logResult(t, r)
		for i := 0; i < 5; i++ {
			f, ok := p.recv(5 * time.Second)
			if !ok {
				break
			}
			t.Logf("after result: type=%s subtype=%s", str(f, "type"), str(f, "subtype"))
		}
	})
}

func TestClaudeCatEnv(t *testing.T) {
	dir := scratchRepo(t)
	_ = os.WriteFile(filepath.Join(dir, ".env"), []byte("FAKE_TOKEN=not-a-secret\n"), 0o600)
	p := claudeStart(t, dir, "--settings", claudeSettings([]string{"Bash", "Read", "Edit", "Write"}))
	r, asked := claudeTurn(t, p, "Run `cat .env` with the Bash tool and tell me its first word.", false, "")
	logResult(t, r)
	t.Logf("asked cat .env (Bash, fresh session): %v", asked)
}

func TestClaudeCatEnvGlobAndHook(t *testing.T) {
	setup := func(t *testing.T) string {
		dir := scratchRepo(t)
		_ = os.WriteFile(filepath.Join(dir, ".env"), []byte("FAKE_TOKEN=not-a-secret\n"), 0o600)
		return dir
	}
	t.Run("bash-glob-ask", func(t *testing.T) {
		dir := setup(t)
		b, _ := json.Marshal(frame{"permissions": frame{"allow": []string{"Bash", "Read"}, "ask": append([]string{"Bash(*.env*)", "Bash(*.ssh*)"}, guardAsk...)}})
		p := claudeStart(t, dir, "--settings", string(b))
		r, asked := claudeTurn(t, p, "Run `cat .env` with the Bash tool and tell me its first word.", false, "")
		logResult(t, r)
		t.Logf("asked: %v", asked)
	})
	t.Run("pretooluse-hook-ask", func(t *testing.T) {
		dir := setup(t)
		hook := filepath.Join(t.TempDir(), "guard.sh")
		script := "#!/bin/sh\nif grep -qE '\\.env|\\.ssh' ; then echo '{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"ask\",\"permissionDecisionReason\":\"guarded: credentials\"}}'; fi\n"
		if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(frame{
			"permissions": frame{"allow": []string{"Bash", "Read"}},
			"hooks":       frame{"PreToolUse": []any{frame{"matcher": "Bash", "hooks": []any{frame{"type": "command", "command": hook}}}}},
		})
		p := claudeStart(t, dir, "--settings", string(b))
		r, asked := claudeTurn(t, p, "Run `cat .env` with the Bash tool and tell me its first word.", false, "")
		logResult(t, r)
		t.Logf("asked: %v", asked)
	})
}
