//go:build integration

package spike

import (
	"os"
	"testing"
	"time"
)

// acpTurn sends session/prompt and drives frames until its response, answering
// session/request_permission with the first option whose kind has the given prefix.
func acpTurn(t *testing.T, p *proc, id int, sid, prompt, pick string) frame {
	t.Helper()
	p.send(frame{"jsonrpc": "2.0", "id": id, "method": "session/prompt", "params": frame{"sessionId": sid, "prompt": []any{frame{"type": "text", "text": prompt}}}})
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		f, ok := p.recv(30 * time.Second)
		if !ok {
			t.Fatal("no frame / eof")
		}
		if str(f, "method") == "session/request_permission" {
			opts, _ := sub(f, "params")["options"].([]any)
			choice := ""
			for _, o := range opts {
				om := o.(map[string]any)
				if str(om, "kind") == pick {
					choice = str(om, "optionId")
				}
			}
			t.Logf("permission request: tool=%s options=%v -> %s(%s)", str(f, "params", "toolCall", "title"), opts, pick, choice)
			resp := frame{"jsonrpc": "2.0", "id": f["id"], "result": frame{"outcome": frame{"outcome": "selected", "optionId": choice}}}
			p.send(resp)
			continue
		}
		if f["id"] == float64(id) && f["method"] == nil {
			return f
		}
	}
	t.Fatal("timeout")
	return nil
}

func TestOpencodeACP(t *testing.T) {
	dir := scratchRepo(t)
	cfg := `{"permission":{"bash":{"*":"allow","rm *":"ask","git reset --hard*":"ask","ssh *":"ask","sudo *":"ask","git push --force*":"ask","cat *.env*":"ask","cat ~/.ssh/*":"ask"},"edit":"allow","read":{"*":"allow","*.env":"ask","*.env.*":"ask"}}}`
	p := start(t, dir, childEnv("OPENCODE_CONFIG_CONTENT="+cfg), "opencode", "acp")
	p.send(frame{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": frame{"protocolVersion": 1, "clientCapabilities": frame{"fs": frame{"readTextFile": false, "writeTextFile": false}, "terminal": false}}})
	init, ok := p.recv(30 * time.Second)
	if !ok {
		t.Fatal("no initialize response")
	}
	t.Logf("initialize result: %v", init["result"])
	p.send(frame{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": frame{"cwd": dir, "mcpServers": []any{}}})
	var sid string
	for i := 0; i < 20 && sid == ""; i++ {
		f, ok := p.recv(30 * time.Second)
		if !ok {
			break
		}
		if f["id"] == float64(2) {
			sid = str(f, "result", "sessionId")
			t.Logf("session/new result: %v", f["result"])
		}
	}
	if sid == "" {
		t.Fatal("no session id")
	}
	model := os.Getenv("SPIKE_OPENCODE_MODEL")
	if model == "" {
		model = "opencode/deepseek-v4.1-flash-free"
	}
	p.send(frame{"jsonrpc": "2.0", "id": 10, "method": "session/set_model", "params": frame{"sessionId": sid, "modelId": model}})
	p.send(frame{"jsonrpc": "2.0", "id": 11, "method": "session/set_config_option", "params": frame{"sessionId": sid, "configId": "model", "value": model}})
	for i := 0; i < 2; {
		f, ok := p.recv(30 * time.Second)
		if !ok {
			t.Fatal("no reply to model switch")
		}
		if f["id"] == float64(10) || f["id"] == float64(11) {
			i++
			t.Logf("model switch id=%v error=%v result-keys=%d", f["id"], f["error"], len(sub(f, "result")))
		}
	}
	r := acpTurn(t, p, 3, sid, "Append the line 'hello from acp' to README.md. Reply 'done' only.", "allow_once")
	t.Logf("turn1 response: %v", r)
	t.Logf("README: %q", readFile(dir, "README.md"))
	r = acpTurn(t, p, 4, sid, "Run `rm victim.txt` with the shell. Reply 'done' only.", "reject_once")
	t.Logf("turn2 response: %v", r)
	t.Logf("victim exists: %q", readFile(dir, "victim.txt"))
}
