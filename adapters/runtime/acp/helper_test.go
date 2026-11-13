package acp_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/panca/haythos/tests/runtimecontract"
)

const helperEnv = "HAYTHOS_ACP_HELPER"

// runHelper is a scripted ACP agent: the test binary re-executed with helperEnv set. The first session/prompt plays
// the script from HAYTHOS_ACP_SCRIPT, later ones just end the turn. HAYTHOS_ACP_LOG, when set, receives one line per
// received frame and one "env" line.
func runHelper() {
	var script runtimecontract.Script
	if raw := os.Getenv("HAYTHOS_ACP_SCRIPT"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &script); err != nil {
			fmt.Fprintln(os.Stderr, "helper: bad script:", err)
			os.Exit(2)
		}
	}
	h := &helper{script: script, responses: map[string]chan map[string]any{}, cancel: make(chan struct{}, 1), out: json.NewEncoder(os.Stdout)}
	h.logf("pid %d", os.Getpid())
	h.logf("env %s", strings.Join(os.Environ(), " "))
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var f map[string]any
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		h.logf("frame %s", sc.Text())
		h.handle(f)
	}
}

type helper struct {
	script runtimecontract.Script
	cwd    string

	mu        sync.Mutex
	out       *json.Encoder
	responses map[string]chan map[string]any
	cancel    chan struct{}
	prompts   int
	nextReq   int
}

func (h *helper) logf(format string, args ...any) {
	path := os.Getenv("HAYTHOS_ACP_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, format+"\n", args...)
}

func (h *helper) send(f map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f["jsonrpc"] = "2.0"
	_ = h.out.Encode(f)
}

func (h *helper) reply(id any, result any) { h.send(map[string]any{"id": id, "result": result}) }

func (h *helper) update(u map[string]any) {
	h.send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "ses_helper", "update": u}})
}

func (h *helper) handle(f map[string]any) {
	method, _ := f["method"].(string)
	id, hasID := f["id"]
	params, _ := f["params"].(map[string]any)
	switch {
	case method == "":
		h.mu.Lock()
		ch := h.responses[fmt.Sprint(id)]
		h.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	case method == "initialize":
		h.reply(id, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true, "promptCapabilities": map[string]any{"image": true}}})
	case method == "session/new":
		h.cwd, _ = params["cwd"].(string)
		h.reply(id, map[string]any{"sessionId": "ses_helper", "configOptions": []any{
			map[string]any{"id": "mode", "category": "mode", "currentValue": "build", "options": []any{map[string]any{"value": "build"}, map[string]any{"value": "plan"}}},
		}})
	case method == "session/load":
		h.cwd, _ = params["cwd"].(string)
		h.update(map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "old", "content": map[string]any{"type": "text", "text": "replayed history"}})
		h.reply(id, map[string]any{})
	case method == "session/set_config_option":
		h.reply(id, map[string]any{"configOptions": []any{}})
	case method == "session/cancel":
		select {
		case h.cancel <- struct{}{}:
		default:
		}
	case method == "session/prompt" && hasID:
		go h.prompt(id)
	case hasID:
		h.send(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "Method not found"}})
	}
}

func (h *helper) prompt(id any) {
	h.mu.Lock()
	first := h.prompts == 0
	h.prompts++
	h.mu.Unlock()
	if !first {
		h.reply(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	allowed := true
	for _, st := range h.script.Steps {
		switch st.Kind {
		case runtimecontract.StepSay:
			for _, c := range st.Chunks {
				h.update(map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m1", "content": map[string]any{"type": "text", "text": c}})
			}
		case runtimecontract.StepAsk:
			ok, cancelled := h.ask(st)
			allowed = allowed && ok
			if cancelled || (!allowed && h.script.RejectEndsTurn) {
				h.reply(id, map[string]any{"stopReason": "cancelled"})
				return
			}
		case runtimecontract.StepWrite:
			if allowed {
				_ = os.WriteFile(filepath.Join(h.cwd, st.Path), []byte(st.Content), 0o600)
			}
		case runtimecontract.StepCrash:
			os.Exit(3)
		case runtimecontract.StepHang:
			<-h.cancel
			h.reply(id, map[string]any{"stopReason": "cancelled"})
			return
		}
	}
	h.reply(id, map[string]any{"stopReason": "end_turn"})
}

// ask sends a permission request and waits for the answer: allowed is true for the allow option, cancelled when the
// client withdrew the request.
func (h *helper) ask(st runtimecontract.Step) (allowed, cancelled bool) {
	h.mu.Lock()
	h.nextReq++
	rid := h.nextReq + 100
	ch := make(chan map[string]any, 1)
	h.responses[fmt.Sprint(rid)] = ch
	h.mu.Unlock()
	h.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tc1", "title": st.Text, "kind": st.ReqKind, "status": "pending"})
	h.send(map[string]any{"id": rid, "method": "session/request_permission", "params": map[string]any{
		"sessionId": "ses_helper",
		"toolCall":  map[string]any{"toolCallId": "tc1", "name": "shell", "title": "title: " + st.Text, "kind": st.ReqKind, "rawInput": map[string]any{"command": st.Text, "cwd": h.cwd}},
		"options": []any{
			map[string]any{"optionId": "once", "kind": "allow_once", "name": "Allow once"},
			map[string]any{"optionId": "always", "kind": "allow_always", "name": "Always"},
			map[string]any{"optionId": "reject", "kind": "reject_once", "name": "Reject"},
		},
	}})
	resp := <-ch
	res, _ := resp["result"].(map[string]any)
	outcome, _ := res["outcome"].(map[string]any)
	if outcome["outcome"] != "selected" {
		return false, true
	}
	return outcome["optionId"] == "once", false
}
