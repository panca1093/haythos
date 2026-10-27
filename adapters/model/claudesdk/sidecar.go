package claudesdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

type SidecarConfig struct {
	Command string
	Args    []string
	Dir     string
	// Env is the complete child environment; it is never inherited, so the token reaches only this process.
	Env []string
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type proc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	resp  chan rpcResponse
	exit  chan struct{}
}

// Sidecar supervises the TypeScript child process: spawn, health check, restart after a crash.
type Sidecar struct {
	cfg    SidecarConfig
	mu     sync.Mutex
	p      *proc
	nextID int
}

func StartSidecar(ctx context.Context, cfg SidecarConfig) (*Sidecar, error) {
	s := &Sidecar{cfg: cfg}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.spawn(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Sidecar) spawn(ctx context.Context) error {
	cmd := exec.Command(s.cfg.Command, s.cfg.Args...) //nolint:gosec,noctx // child outlives the call ctx; killed by Close or on crash
	cmd.Dir, cmd.Env = s.cfg.Dir, s.cfg.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("sidecar stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("sidecar stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sidecar: %w", err)
	}
	p := &proc{cmd: cmd, stdin: stdin, resp: make(chan rpcResponse, 1), exit: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var r rpcResponse
			if json.Unmarshal(sc.Bytes(), &r) == nil {
				p.resp <- r
			}
		}
		_ = cmd.Wait()
		close(p.exit)
	}()
	s.p = p
	if _, err := s.call(ctx, "ping", nil); err != nil {
		s.kill()
		return fmt.Errorf("sidecar health check: %w", err)
	}
	return nil
}

func (s *Sidecar) kill() {
	if s.p != nil {
		_ = s.p.cmd.Process.Kill()
		_ = s.p.stdin.Close()
		<-s.p.exit
		s.p = nil
	}
}

func (s *Sidecar) alive() bool {
	if s.p == nil {
		return false
	}
	select {
	case <-s.p.exit:
		return false
	default:
		return true
	}
}

func (s *Sidecar) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.nextID++
	id := s.nextID
	b, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", method, err)
	}
	p := s.p
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}
	for {
		select {
		case r := <-p.resp:
			if r.ID != id {
				continue
			}
			if r.Error != nil {
				return nil, fmt.Errorf("sidecar %s: %s", method, r.Error.Message)
			}
			return r.Result, nil
		case <-p.exit:
			return nil, fmt.Errorf("sidecar exited during %s", method)
		case <-ctx.Done():
			s.kill()
			return nil, fmt.Errorf("sidecar %s: %w", method, ctx.Err())
		}
	}
}

var errSidecarDown = errors.New("sidecar down")

// callWithRestart runs a side-effect-free call; one retry after a crash is safe.
func (s *Sidecar) callWithRestart(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if !s.alive() {
			s.kill()
			if err := s.spawn(ctx); err != nil {
				return nil, fmt.Errorf("restart sidecar: %w", err)
			}
		}
		raw, err := s.call(ctx, method, params)
		if err == nil {
			return raw, nil
		}
		if ctx.Err() != nil || s.alive() {
			return nil, err
		}
		lastErr = fmt.Errorf("%w: %w", errSidecarDown, err)
	}
	return nil, lastErr
}

// Step implements Transport.
func (s *Sidecar) Step(ctx context.Context, req StepRequest) (StepResponse, error) {
	raw, err := s.callWithRestart(ctx, "step", req)
	if err != nil {
		return StepResponse{}, err
	}
	var resp StepResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return StepResponse{}, fmt.Errorf("decode step result: %w", err)
	}
	return resp, nil
}

// Models asks the SDK for the live model list; it sends no model turn.
func (s *Sidecar) Models(ctx context.Context) ([]ModelInfo, error) {
	raw, err := s.callWithRestart(ctx, "models", nil)
	if err != nil {
		return nil, err
	}
	var models []ModelInfo
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, fmt.Errorf("decode models result: %w", err)
	}
	return models, nil
}

func (s *Sidecar) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kill()
	return nil
}
