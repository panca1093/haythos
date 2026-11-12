package readfile

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gradient(w, h int) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.SetNRGBA(x, y, color.NRGBA{uint8(x * 255 / w), uint8(y * 255 / h), 128, 255})
		}
	}
	return m
}

func noisy(w, h int, alpha uint8) *image.NRGBA {
	rng := rand.New(rand.NewSource(1))
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(m.Pix); i += 4 {
		m.Pix[i], m.Pix[i+1], m.Pix[i+2], m.Pix[i+3] = uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), alpha
	}
	return m
}

func encodePNG(t *testing.T, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeJPEG(t *testing.T, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	p := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	var b bytes.Buffer
	if err := gif.Encode(&b, p, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func hugePNGHeader(w, h uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 2
	chunk := append([]byte("IHDR"), ihdr...)
	_ = binary.Write(&b, binary.BigEndian, uint32(13))
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func TestReadImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		data      []byte
		wantMedia string
		wantW     int
		wantH     int
		wantSum   []string
		wantErr   string
	}{
		{"small png unchanged", encodePNG(t, gradient(200, 100)), "image/png", 200, 100, []string{"image 200×100,", "png"}, ""},
		{"large png scaled", encodePNG(t, gradient(3000, 1000)), "image/png", 1568, 523, []string{"image 1568×523 (from 3000×1000)"}, ""},
		{"tall png scaled", encodePNG(t, gradient(1000, 3000)), "image/png", 523, 1568, []string{"(from 1000×3000)"}, ""},
		{"jpeg stays jpeg", encodeJPEG(t, gradient(300, 200)), "image/jpeg", 300, 200, []string{"jpeg"}, ""},
		{"gif becomes png", encodeGIF(t, 50, 40), "image/png", 50, 40, []string{"png"}, ""},
		{"noisy transparent png falls back to jpeg", encodePNG(t, noisy(800, 800, 200)), "image/jpeg", 800, 800, []string{"jpeg"}, ""},
		{"over 40 megapixels refused", hugePNGHeader(10000, 10000), "", 0, 0, nil, "too many pixels"},
		{"random bytes refused", bytes.Repeat([]byte{0x12, 0x34, 0x56}, 100), "", 0, 0, nil, "not a png, jpeg or gif image"},
		{"webp-like refused", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "", 0, 0, nil, "not a png, jpeg or gif image"},
		{"empty refused", nil, "", 0, 0, nil, "not a png, jpeg or gif image"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			img, summary, err := readImage(bytes.NewReader(tc.data))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if img.MediaType != tc.wantMedia {
				t.Errorf("media = %q, want %q", img.MediaType, tc.wantMedia)
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(img.Data))
			if err != nil {
				t.Fatalf("output does not decode: %v", err)
			}
			if cfg.Width != tc.wantW || cfg.Height != tc.wantH {
				t.Errorf("size = %dx%d, want %dx%d", cfg.Width, cfg.Height, tc.wantW, tc.wantH)
			}
			for _, s := range tc.wantSum {
				if !strings.Contains(summary, s) {
					t.Errorf("summary %q missing %q", summary, s)
				}
			}
			if strings.Contains(tc.name, "unchanged") && strings.Contains(summary, "from") {
				t.Errorf("summary %q should not mention a resize", summary)
			}
		})
	}
}

func TestReadImageTransparentJPEGFallbackCompositesOnWhite(t *testing.T) {
	t.Parallel()
	img, _, err := readImage(bytes.NewReader(encodePNG(t, noisy(800, 800, 0))))
	if err != nil {
		t.Fatal(err)
	}
	if img.MediaType != "image/jpeg" {
		t.Skipf("fully transparent png fit in 1 MiB as %s", img.MediaType)
	}
	out, err := jpeg.Decode(bytes.NewReader(img.Data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := out.At(400, 400).RGBA()
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Errorf("transparent pixel = %d,%d,%d, want near white", r>>8, g>>8, b>>8)
	}
}

func TestReadImageDeterministic(t *testing.T) {
	t.Parallel()
	data := encodePNG(t, gradient(2000, 500))
	a, _, err := readImage(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := readImage(bytes.NewReader(data))
	if !bytes.Equal(a.Data, b.Data) {
		t.Error("output differs between runs")
	}
}

func TestReadImageTooLargeEvenAfterDownscaling(t *testing.T) {
	t.Parallel()
	_, _, err := readImage(bytes.NewReader(encodePNG(t, noisy(1568, 1568, 255))))
	if err == nil || !strings.Contains(err.Error(), "image is too large even after downscaling") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadFileImageEndToEnd(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "shot.png"), encodePNG(t, gradient(64, 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, f.root, "file.read", `{"path":"shot.png"}`)
	if res.IsError {
		t.Fatalf("refused: %s", res.Content)
	}
	if len(res.Images) != 1 || res.Images[0].MediaType != "image/png" {
		t.Fatalf("images = %+v", res.Images)
	}
	if !res.Untrusted {
		t.Error("image result must be untrusted")
	}
	if !strings.Contains(res.Content, "shot.png") {
		t.Errorf("content = %q", res.Content)
	}
}
