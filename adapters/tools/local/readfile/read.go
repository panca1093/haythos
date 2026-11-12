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
	"path/filepath"
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
	binarySniff  = 8 << 10
)

var readSpec = task.ToolSpec{
	Name:        "file.read",
	Description: "Read a file in the chat folder. Text files, PDF, docx, xlsx and pptx come back as numbered lines: search first with file.search (it does not look inside documents), then read only the lines you need with offset/limit. Returns up to 2000 lines or 32 KiB; continue with the offset it gives. png, jpeg and gif come back as an image you can see.",
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
	ranged := a.Offset != nil || a.Limit != nil
	ext := strings.ToLower(filepath.Ext(target.Full))
	if msg, ok := unsupported[ext]; ok {
		return refuse(msg)
	}
	if imageExts[ext] {
		if opened.Size() > maxImageFile {
			return refuse(fmt.Sprintf("image is %d MiB; the limit is %d MiB", opened.Size()>>20, maxImageFile>>20))
		}
		img, summary, err := readImage(f)
		if err != nil {
			return refuse(err.Error())
		}
		return task.ToolResult{Content: target.Rel + ": " + summary, Images: []task.Image{img}, Untrusted: true}, nil
	}
	if extract, ok := t.extractor(ext); ok {
		if opened.Size() > maxDocFile {
			return refuse(fmt.Sprintf("document is %d MiB; the limit is %d MiB", opened.Size()>>20, maxDocFile>>20))
		}
		text, err := extract(ctx, f, opened.Size())
		if err != nil {
			return refuse(err.Error())
		}
		if len(text) > bigFile && !ranged {
			return refuse(fmt.Sprintf("document text is %d KiB; use offset/limit", len(text)>>10))
		}
		out, err := numberedLines(strings.NewReader(text), offset, limit)
		if err != nil {
			return refuse(err.Error())
		}
		return task.ToolResult{Content: task.StripInvisible(out), Untrusted: true}, nil
	}
	if opened.Size() > bigFile && !ranged {
		return refuse(fmt.Sprintf("file is %d KiB; use file.search or offset/limit", opened.Size()>>10))
	}
	head := make([]byte, binarySniff)
	n, err := f.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return refuse("not readable")
	}
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return refuse("not text (binary file)")
	}
	out, err := numberedLines(f, offset, limit)
	if err != nil {
		return refuse(err.Error())
	}
	return task.ToolResult{Content: task.StripInvisible(out), Untrusted: !IsSource(target.Full)}, nil
}

const (
	maxImageFile = 20 << 20
	maxDocFile   = 50 << 20
)

var (
	imageExts   = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true}
	unsupported = map[string]string{
		".xls": oldOffice, ".doc": oldOffice, ".ppt": oldOffice,
		".webp": otherImage, ".heic": otherImage, ".heif": otherImage, ".bmp": otherImage, ".tif": otherImage, ".tiff": otherImage, ".svg": otherImage,
	}
)

const (
	oldOffice  = "old binary Office format; save it as .xlsx/.docx/.pptx"
	otherImage = "image format not supported; convert to png or jpeg"
)

// extractor returns the text extractor for a document extension; f is the opened file and size its length.
func (t Guard) extractor(ext string) (func(ctx context.Context, f *os.File, size int64) (string, error), bool) {
	switch ext {
	case ".pdf":
		return t.pdfText, true
	case ".docx", ".xlsx", ".pptx":
		return func(_ context.Context, f *os.File, size int64) (string, error) { return officeText(f, size, ext) }, true
	}
	return nil, false
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
	for i := 0; cut && i < utf8.UTFMax-1 && len(buf) > 0; i++ {
		if r, size := utf8.DecodeLastRune(buf); r != utf8.RuneError || size != 1 {
			break
		}
		buf = buf[:len(buf)-1]
	}
	text := strings.ToValidUTF8(string(buf), "\uFFFD")
	if utf8.RuneCountInString(text) > maxLineChars {
		cut = true
		text = string([]rune(text)[:maxLineChars])
	}
	if cut {
		text += "… [line truncated]"
	}
	return text, nil
}
