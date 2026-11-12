package readfile

import (
	"errors"
	"io"

	"github.com/panca/haythos/core/task"
)

// readImage decodes a png, jpeg or gif, downscales it and re-encodes it; summary describes the result for text-only paths.
func readImage(r io.Reader) (img task.Image, summary string, err error) {
	return task.Image{}, "", errors.New("images are not supported yet")
}
