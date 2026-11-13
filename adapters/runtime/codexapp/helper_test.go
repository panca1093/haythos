package codexapp_test

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

const helperEnv = "HAYTHOS_CODEX_HELPER"

// runHelper is a scripted codex app-server: the test binary re-executed with helperEnv set. The first turn/start
// plays the script from HAYTHOS_CODEX_SCRIPT, or the raw notifications in HAYTHOS_CODEX_RAW, later ones just end the
// turn. HAYTHOS_CODEX_LOG, when set, receives one line per received frame and one "env" line. Like the real server,
// its frames carry no "jsonrpc" field.
func runHelper() {
	var script runtimecontract.Script
	if raw := os.Getenv("HAYTHOS_CODEX_SCRIPT"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &script); err != nil {
			fmt.Fprintln(os.Stderr, "helper: bad script:", err)
			os.Exit(2)
		}
	}
	h := &helper{script: script, responses: map[string]chan map[string]any{}, interrupt: make(chan struct{}, 1), out: json.NewEncoder(os.Stdout)}
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
	interrupt chan struct{}
	turns     int
	nextReq   int
}

func (h *helper) logf(format string, args ...any) {
	path := os.Getenv("HAYTHOS_CODEX_LOG")
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
	_ = h.out.Encode(f)
}

func (h *helper) reply(id any, result any) { h.send(map[string]any{"id": id, "result": result}) }

func (h *helper) notify(method string, params map[string]any) {
	params["threadId"] = "thr_helper"
	h.send(map[string]any{"method": method, "params": params})
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
		h.reply(id, map[string]any{"codexHome": os.Getenv("CODEX_HOME"), "platformOs": "macos", "userAgent": "helper"})
	case method == "thread/start":
		h.cwd, _ = params["cwd"].(string)
		h.reply(id, map[string]any{"thread": map[string]any{"id": "thr_helper"}})
	case method == "turn/interrupt":
		select {
		case h.interrupt <- struct{}{}:
		default:
		}
		h.reply(id, map[string]any{})
	case method == "turn/start" && hasID:
		h.reply(id, map[string]any{"turn": map[string]any{"id": "turn_1", "status": "inProgress"}})
		go h.turn()
	case hasID:
		h.send(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "Method not found"}})
	}
}

func (h *helper) complete(status string) {
	h.notify("turn/completed", map[string]any{"turn": map[string]any{"id": "turn_1", "status": status, "error": map[string]any{"message": "boom"}}})
}

func (h *helper) turn() {
	h.mu.Lock()
	first := h.turns == 0
	h.turns++
	h.mu.Unlock()
	h.notify("turn/started", map[string]any{"turn": map[string]any{"id": "turn_1", "status": "inProgress"}})
	if !first {
		h.complete("completed")
		return
	}
	if raw := os.Getenv("HAYTHOS_CODEX_RAW"); raw != "" {
		var frames []map[string]any
		if err := json.Unmarshal([]byte(raw), &frames); err != nil {
			fmt.Fprintln(os.Stderr, "helper: bad raw frames:", err)
			os.Exit(2)
		}
		for _, f := range frames {
			h.send(f)
		}
		h.complete(os.Getenv("HAYTHOS_CODEX_STATUS"))
		return
	}
	allowed := true
	for _, st := range h.script.Steps {
		switch st.Kind {
		case runtimecontract.StepSay:
			for _, c := range st.Chunks {
				h.notify("item/agentMessage/delta", map[string]any{"itemId": "m1", "turnId": "turn_1", "delta": c})
			}
		case runtimecontract.StepAsk:
			ok, cancelled := h.ask(st)
			allowed = allowed && ok
			if cancelled || (!allowed && h.script.RejectEndsTurn) {
				h.complete("interrupted")
				return
			}
		case runtimecontract.StepWrite:
			if allowed {
				_ = os.WriteFile(filepath.Join(h.cwd, st.Path), []byte(st.Content), 0o600)
			}
		case runtimecontract.StepCrash:
			os.Exit(3)
		case runtimecontract.StepHang:
			<-h.interrupt
			h.complete("interrupted")
			return
		}
	}
	h.complete("completed")
}

// ask sends an approval request and waits for the answer: allowed is true for accept, cancelled when the client
// answered cancel.
func (h *helper) ask(st runtimecontract.Step) (allowed, cancelled bool) {
	h.mu.Lock()
	h.nextReq++
	rid := h.nextReq - 1
	ch := make(chan map[string]any, 1)
	h.responses[fmt.Sprint(rid)] = ch
	h.mu.Unlock()
	method := "item/commandExecution/requestApproval"
	params := map[string]any{"itemId": "it1", "turnId": "turn_1", "threadId": "thr_helper", "command": st.Text, "cwd": h.cwd, "availableDecisions": []any{"accept", "cancel"}}
	item := map[string]any{"id": "it1", "type": "commandExecution", "command": st.Text, "status": "inProgress"}
	if st.ReqKind == "edit" {
		method = "item/fileChange/requestApproval"
		params = map[string]any{"itemId": "it1", "turnId": "turn_1", "threadId": "thr_helper"}
		item = map[string]any{"id": "it1", "type": "fileChange", "status": "inProgress", "changes": []any{map[string]any{"path": st.Text, "kind": "update", "diff": ""}}}
	}
	h.notify("item/started", map[string]any{"item": item})
	h.send(map[string]any{"id": rid, "method": method, "params": params})
	resp := <-ch
	res, _ := resp["result"].(map[string]any)
	switch res["decision"] {
	case "accept":
		return true, false
	case "cancel":
		return false, true
	}
	return false, false
}
