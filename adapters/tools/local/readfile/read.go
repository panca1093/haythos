package readfile

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unicode/utf8"

	"github.com/panca/haythos/core/task"
)

var readSpec = task.ToolSpec{
	Name:        "file.read",
	Description: "Read up to 8 KiB from a text file. Use file.search first and pass a 1-based start_line and end_line for relevant lines.",
	InputSchema: json.RawMessage(pathArg + `,"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["path"]}`),
}

func (t Guard) read(ctx context.Context, raw []byte) (task.ToolResult, error) {
	target, why := t.resolve(ctx, raw)
	if why != "" {
		return refuse(why)
	}
	var a struct {
		Start int `json:"start_line"`
		End   int `json:"end_line"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Start < 0 || a.End < 0 || (a.End > 0 && a.End < a.Start) {
		return refuse("use a valid 1-based line range")
	}
	resolved, err := os.Lstat(target.Full)
	if err != nil || !resolved.Mode().IsRegular() {
		return refuse("not a regular file")
	}
	root, err := os.OpenRoot(target.Root)
	if err != nil {
		return refuse("not readable")
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(target.Rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return refuse("not readable")
	}
	defer func() { _ = f.Close() }()
	if opened, err := f.Stat(); err != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, resolved) {
		return refuse("not readable")
	}
	var b []byte
	var errRead error
	if a.Start > 0 || a.End > 0 {
		b, errRead = readLines(f, max(1, a.Start), a.End)
	} else {
		b, errRead = io.ReadAll(io.LimitReader(f, maxRead+1))
	}
	if errRead != nil {
		return refuse(errRead.Error())
	}
	truncated := len(b) > maxRead
	if truncated {
		b = b[:maxRead]
		for i := 0; i < utf8.UTFMax-1 && len(b) > 0 && !utf8.Valid(b); i++ {
			b = b[:len(b)-1]
		}
	}
	if !utf8.Valid(b) {
		return refuse("not text (binary file)")
	}
	out := string(b)
	if truncated {
		out += fmt.Sprintf("\n[truncated at %d KiB; use file.search or request a smaller line range]", maxRead>>10)
	}
	return task.ToolResult{Content: task.StripInvisible(out), Untrusted: !IsSource(target.Full)}, nil
}

func readLines(f io.Reader, start, end int) ([]byte, error) {
	bounded := &io.LimitedReader{R: f, N: maxScan + 1}
	scanner := bufio.NewScanner(bounded)
	var out []byte
	for line := 1; scanner.Scan(); line++ {
		if line < start {
			continue
		}
		if line > start {
			out = append(out, '\n')
		}
		out = append(out, scanner.Bytes()[:min(len(scanner.Bytes()), maxRead+1-len(out))]...)
		if line == end || len(out) > maxRead {
			return out, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("cannot read requested lines: %w", err)
	}
	if bounded.N == 0 && len(out) <= maxRead {
		return nil, errors.New("requested range exceeds the 1 MiB scan limit")
	}
	return out, nil
}
