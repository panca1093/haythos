package readfile

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/panca/haythos/core/task"
)

const (
	defaultLimit = 2000
	maxLineChars = 2000
	maxOutput    = 32 << 10
	bigFile      = 256 << 10
)

var readSpec = task.ToolSpec{
	Name:        "file.read",
	Description: "Read a text file with line numbers. Search first with file.search, then read only the lines you need with offset/limit. Returns up to 2000 lines or 32 KiB; continue with the offset it gives.",
	InputSchema: json.RawMessage(pathArg + `,"offset":{"type":"integer","minimum":1,"description":"1-based first line (default 1)"},"limit":{"type":"integer","minimum":1,"description":"number of lines (default 2000)"}},"required":["path"]}`),
}

func (t Guard) read(ctx context.Context, raw []byte) (task.ToolResult, error) {
	target, why := t.resolve(ctx, raw)
	if why != "" {
		return refuse(why)
	}
	var a struct {
		Offset *int `json:"offset"`
		Limit  *int `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || (a.Offset != nil && *a.Offset < 0) || (a.Limit != nil && *a.Limit < 1) {
		return refuse("offset and limit must be positive integers")
	}
	offset, limit := 1, defaultLimit
	if a.Offset != nil {
		offset = max(1, *a.Offset)
	}
	if a.Limit != nil {
		limit = *a.Limit
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
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, resolved) {
		return refuse("not readable")
	}
	if opened.Size() > bigFile && a.Offset == nil && a.Limit == nil {
		return refuse(fmt.Sprintf("file is %d KiB; use file.search or offset/limit", opened.Size()>>10))
	}
	out, err := numberedLines(f, offset, limit)
	if err != nil {
		return refuse(err.Error())
	}
	return task.ToolResult{Content: task.StripInvisible(out), Untrusted: !IsSource(target.Full)}, nil
}

func numberedLines(f io.Reader, offset, limit int) (string, error) {
	r := bufio.NewReaderSize(f, 64<<10)
	var out strings.Builder
	line := 0
	for {
		text, err := nextLine(r)
		if errors.Is(err, io.EOF) {
			if offset > 1 && line < offset {
				return fmt.Sprintf("[offset %d is beyond the end of the file (%d lines)]", offset, line), nil
			}
			return out.String(), nil
		}
		if err != nil {
			return "", err
		}
		line++
		if line < offset {
			continue
		}
		entry := fmt.Sprintf("%d|%s", line, text)
		if line >= offset+limit || out.Len()+len(entry)+1 > maxOutput {
			return fmt.Sprintf("%s\n[truncated; continue with offset=%d]", strings.TrimSuffix(out.String(), "\n"), line), nil
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(entry)
	}
}

func nextLine(r *bufio.Reader) (string, error) {
	const keep = maxLineChars * utf8.UTFMax
	var buf []byte
	cut, seen := false, false
	for {
		chunk, err := r.ReadSlice('\n')
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
			return "", fmt.Errorf("read file: %w", err)
		}
		seen = seen || len(chunk) > 0
		chunk = bytes.TrimSuffix(chunk, []byte("\n"))
		room := keep - len(buf)
		buf = append(buf, chunk[:min(room, len(chunk))]...)
		cut = cut || len(chunk) > room
		if !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) && !seen {
				return "", io.EOF
			}
			break
		}
	}
	buf = bytes.TrimSuffix(buf, []byte("\r"))
	for i := 0; cut && i < utf8.UTFMax-1 && len(buf) > 0 && !utf8.Valid(buf); i++ {
		buf = buf[:len(buf)-1]
	}
	if !utf8.Valid(buf) {
		return "", errors.New("not text (binary file)")
	}
	text := string(buf)
	if utf8.RuneCountInString(text) > maxLineChars {
		cut = true
		text = string([]rune(text)[:maxLineChars])
	}
	if cut {
		text += "… [line truncated]"
	}
	return text, nil
}
