package readfile

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	"github.com/panca/haythos/core/task"
)

const (
	maxImageEdge   = 1568
	maxImagePixels = 40_000_000
	maxImageOutput = 1 << 20
	imageReadLimit = 21 << 20
)

// readImage decodes a png, jpeg or gif, downscales it and re-encodes it; summary describes the result for text-only paths.
func readImage(r io.Reader) (img task.Image, summary string, err error) {
	raw, err := io.ReadAll(io.LimitReader(r, imageReadLimit))
	if err != nil {
		return task.Image{}, "", fmt.Errorf("read image: %w", err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif") {
		return task.Image{}, "", errors.New("not a png, jpeg or gif image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return task.Image{}, "", fmt.Errorf("image is %d×%d: too many pixels (limit %d megapixels)", cfg.Width, cfg.Height, maxImagePixels/1_000_000)
	}
	src, err := decodeFirstFrame(raw, format)
	if err != nil {
		return task.Image{}, "", errors.New("image is corrupt or truncated")
	}

	out := toRGBA(src)
	w, h := out.Bounds().Dx(), out.Bounds().Dy()
	if long := max(w, h); long > maxImageEdge {
		nw, nh := max(1, (w*maxImageEdge+long/2)/long), max(1, (h*maxImageEdge+long/2)/long)
		out = boxResize(out, nw, nh)
	}

	data, media, err := encodeImage(out, format == "jpeg")
	if err != nil {
		return task.Image{}, "", err
	}
	b := out.Bounds()
	summary = fmt.Sprintf("image %d×%d", b.Dx(), b.Dy())
	if b.Dx() != w || b.Dy() != h {
		summary += fmt.Sprintf(" (from %d×%d)", w, h)
	}
	summary += fmt.Sprintf(", %d KiB %s", (len(data)+1023)>>10, media[len("image/"):])
	return task.Image{MediaType: media, Data: data}, summary, nil
}

func decodeFirstFrame(raw []byte, format string) (image.Image, error) {
	switch format {
	case "png":
		return png.Decode(bytes.NewReader(raw))
	case "jpeg":
		return jpeg.Decode(bytes.NewReader(raw))
	default:
		return gif.Decode(bytes.NewReader(raw))
	}
}

func toRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

func boxResize(src *image.RGBA, nw, nh int) *image.RGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := y*sh/nh, max((y+1)*sh/nh, y*sh/nh+1)
		for x := 0; x < nw; x++ {
			x0, x1 := x*sw/nw, max((x+1)*sw/nw, x*sw/nw+1)
			var r, g, b, a, n int
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+4]
					r, g, b, a, n = r+int(p[0]), g+int(p[1]), b+int(p[2]), a+int(p[3]), n+1
				}
			}
			o := dst.Pix[y*dst.Stride+x*4:]
			//nolint:gosec // averages of uint8 samples fit in uint8
			o[0], o[1], o[2], o[3] = uint8((r+n/2)/n), uint8((g+n/2)/n), uint8((b+n/2)/n), uint8((a+n/2)/n)
		}
	}
	return dst
}

func encodeImage(m *image.RGBA, jpegSource bool) ([]byte, string, error) {
	if !jpegSource {
		var buf bytes.Buffer
		if err := png.Encode(&buf, m); err != nil {
			return nil, "", fmt.Errorf("encode png: %w", err)
		}
		if buf.Len() <= maxImageOutput {
			return buf.Bytes(), "image/png", nil
		}
	}
	flat := image.NewRGBA(m.Bounds())
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), m, m.Bounds().Min, draw.Over)
	for _, q := range []int{85, 70} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: q}); err != nil {
			return nil, "", fmt.Errorf("encode jpeg: %w", err)
		}
		if buf.Len() <= maxImageOutput {
			return buf.Bytes(), "image/jpeg", nil
		}
	}
	return nil, "", errors.New("image is too large even after downscaling")
}
