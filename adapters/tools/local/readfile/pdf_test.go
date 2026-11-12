package readfile

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panca/haythos/adapters/tools/local"
)

func testPDF(pages ...string) []byte {
	objs := []string{"<< /Type /Catalog /Pages 2 0 R >>"}
	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	objs = append(objs,
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, text := range pages {
		stream := ""
		if text != "" {
			stream = fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", text)
		}
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", 5+2*i),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func pdfFixture(t *testing.T, cfg Config, body []byte) (fixture, string) {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "chat")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Home = home
	reg := local.New()
	Register(reg, cfg)
	return fixture{reg: reg, home: home, root: root}, root
}

func needPdftotext(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
}

func TestPDFRead(t *testing.T) {
	t.Parallel()
	needPdftotext(t)
	tests := []struct {
		name    string
		body    []byte
		want    string
		wantErr string
	}{
		{"two pages", testPDF("Hello PDF", "Page two"), "1|# page 1\n2|Hello PDF\n3|# page 2\n4|Page two", ""},
		{"no text layer", testPDF(""), "", "no text layer"},
		{"garbage", []byte("this is not a pdf at all"), "", "pdftotext could not read this PDF"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, root := pdfFixture(t, Config{}, tc.body)
			res := f.call(t, root, "file.read", `{"path":"doc.pdf"}`)
			if tc.wantErr != "" {
				if !res.IsError || !strings.Contains(res.Content, tc.wantErr) {
					t.Fatalf("got %+v, want error containing %q", res, tc.wantErr)
				}
				return
			}
			if res.IsError || !res.Untrusted {
				t.Fatalf("got %+v, want untrusted success", res)
			}
			if res.Content != tc.want {
				t.Fatalf("content = %q, want %q", res.Content, tc.want)
			}
		})
	}
}

func TestPDFMissingBinary(t *testing.T) {
	t.Parallel()
	f, root := pdfFixture(t, Config{Pdftotext: filepath.Join(t.TempDir(), "nope")}, testPDF("Hello PDF"))
	res := f.call(t, root, "file.read", `{"path":"doc.pdf"}`)
	if !res.IsError || !strings.Contains(res.Content, "pdftotext not found") {
		t.Fatalf("got %+v", res)
	}
}
