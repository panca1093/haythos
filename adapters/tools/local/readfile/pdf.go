package readfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	pdfTimeout = 30 * time.Second
	pdfMaxText = 16 << 20
)

// pdfText extracts a PDF's text with pdftotext, feeding the opened file on stdin.
func (t Guard) pdfText(ctx context.Context, f *os.File, _ int64) (string, error) {
	bin := t.cfg.Pdftotext
	if bin == "" {
		bin = "pdftotext"
	}
	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()
	out, over, err := runStdin(ctx, "", []string{bin, "-q", "-layout", "-enc", "UTF-8", "-", "-"}, nil, pdfMaxText, f)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return "", errors.New("pdftotext not found; install poppler (brew install poppler)")
		}
		return "", errors.New("pdftotext could not read this PDF")
	}
	if strings.TrimSpace(strings.ReplaceAll(out, "\f", "")) == "" {
		return "", errors.New("no text layer (scanned PDF?)")
	}
	pages := strings.Split(out, "\f")
	if len(pages) > 1 && strings.TrimSpace(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	var b strings.Builder
	for i, p := range pages {
		fmt.Fprintf(&b, "# page %d\n%s", i+1, strings.TrimRight(p, "\n"))
		b.WriteByte('\n')
	}
	if over {
		b.WriteString("[pdf text truncated at 16 MiB]\n")
	}
	return b.String(), nil
}
