package readfile

import (
	"context"
	"errors"
	"os"
)

// pdfText extracts a PDF's text with pdftotext, feeding the opened file on stdin.
func (t Guard) pdfText(ctx context.Context, f *os.File, size int64) (string, error) {
	return "", errors.New("pdf is not supported yet")
}
