//go:build integration

package spike

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type codexClient struct {
	t    *testing.T
	p    *proc
	next int
}

func (c *codexClient) call(method string, params frame) frame {
	c.next++
	id := c.next
	c.p.send(frame{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	for {
		f, ok := c.p.recv(60 * time.Second)
		if !ok {
			c.t.Fatalf("no response to %s", method)
		}
		if f["id"] == float64(id) && f["method"] == nil {
			return f
		}
	}
}

// codexTurn starts a turn and answers command approval requests with decision until turn/completed.
func (c *codexClient) turn(threadID, prompt, decision string) (completed frame, asked []string) {
	c.next++
	id := c.next
	c.p.send(frame{"jsonrpc": "2.0", "id": id, "method": "turn/start", "params": frame{"threadId": threadID, "input": []any{frame{"type": "text", "text": prompt}}}})
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		f, ok := c.p.recv(60 * time.Second)
		if !ok {
			c.t.Fatal("no frame / eof")
		}
		m := str(f, "method")
		if m != "" && f["id"] != nil {
			c.t.Logf("server request: %s params=%s", m, trim(str(f, "params"), 500))
			if m == "item/commandExecution/requestApproval" || m == "item/fileChange/requestApproval" {
				asked = append(asked, m)
				c.p.send(frame{"jsonrpc": "2.0", "id": f["id"], "result": frame{"decision": decision}})
			} else {
				c.p.send(frame{"jsonrpc": "2.0", "id": f["id"], "error": frame{"code": -32601, "message": "unsupported"}})
			}
			continue
		}
		if m == "turn/completed" {
			return f, asked
		}
	}
	c.t.Fatal("timeout")
	return nil, nil
}

func TestCodexAppServer(t *testing.T) {
	dir := scratchRepo(t)
	p := start(t, dir, childEnv(), "codex", "app-server")
	c := &codexClient{t: t, p: p}
	r := c.call("initialize", frame{"clientInfo": frame{"name": "haythos-spike", "version": "0"}, "capabilities": frame{"experimentalApi": true}})
	t.Logf("initialize: %v", r["result"])
	p.send(frame{"jsonrpc": "2.0", "method": "initialized"})
	r = c.call("thread/start", frame{"cwd": dir, "approvalPolicy": "untrusted", "sandbox": "workspace-write", "ephemeral": true})
	if r["error"] != nil {
		t.Fatalf("thread/start: %v", r["error"])
	}
	tid := str(r, "result", "thread", "id")
	t.Logf("thread/start keys ok, thread=%s approvalPolicy=%v sandbox=%v", tid, sub(r, "result")["approvalPolicy"], sub(r, "result")["sandbox"])
	done, asked := c.turn(tid, "Append the line 'hello from codex' to README.md using a shell command. Reply 'done' only.", "accept")
	t.Logf("turn1 completed: %s; asked=%v; README=%q", trim(str(done, "params"), 400), asked, readFile(dir, "README.md"))
	done, asked = c.turn(tid, "Run `rm victim.txt` with the shell. Reply 'done' only.", "decline")
	t.Logf("turn2 completed: %s; asked=%v; victim=%q", trim(str(done, "params"), 400), asked, readFile(dir, "victim.txt"))
}

func TestCodexGuardRules(t *testing.T) {
	dir := scratchRepo(t)
	rules := filepath.Join(dir, ".codex", "rules")
	if err := os.MkdirAll(rules, 0o755); err != nil {
		t.Fatal(err)
	}
	guard := `prefix_rule(pattern=["rm"], decision="prompt", justification="guarded: delete")
prefix_rule(pattern=["git", "reset", "--hard"], decision="prompt")
prefix_rule(pattern=["ssh"], decision="prompt")
prefix_rule(pattern=["sudo"], decision="prompt")
prefix_rule(pattern=["git", "push", "--force"], decision="prompt")
`
	if err := os.WriteFile(filepath.Join(rules, "guard.rules"), []byte(guard), 0o644); err != nil {
		t.Fatal(err)
	}
	p := start(t, dir, childEnv(), "codex", "-c", `projects."`+dir+`".trust_level="trusted"`, "app-server")
	c := &codexClient{t: t, p: p}
	c.call("initialize", frame{"clientInfo": frame{"name": "haythos-spike", "version": "0"}, "capabilities": frame{"experimentalApi": true}})
	p.send(frame{"jsonrpc": "2.0", "method": "initialized"})
	r := c.call("thread/start", frame{"cwd": dir, "approvalPolicy": "on-request", "sandbox": "danger-full-access", "ephemeral": true})
	if r["error"] != nil {
		t.Fatalf("thread/start: %v", r["error"])
	}
	tid := str(r, "result", "thread", "id")
	_, asked := c.turn(tid, "Append the line 'hello from codex' to README.md using a shell command. Reply 'done' only.", "accept")
	t.Logf("turn1 asked (want none): %v; README=%q", asked, readFile(dir, "README.md"))
	_, asked = c.turn(tid, "Run `rm victim.txt` with the shell. Reply 'done' only.", "decline")
	t.Logf("turn2 asked (rule says prompt): %v; victim=%q", asked, readFile(dir, "victim.txt"))
}
