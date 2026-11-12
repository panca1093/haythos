package readfile

import (
	"errors"
	"io"
)

// officeText extracts the text of a docx, xlsx or pptx (ext includes the dot).
func officeText(r io.ReaderAt, size int64, ext string) (string, error) {
	return "", errors.New(ext + " is not supported yet")
}
