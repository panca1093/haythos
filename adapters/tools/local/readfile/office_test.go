package readfile

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testNSW   = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`
	testNSS   = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	testNSP   = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	testRelNS = `xmlns="http://schemas.openxmlformats.org/package/2006/relationships"`
)

func buildZip(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func docxParts(body string) map[string]string {
	return map[string]string{"word/document.xml": `<w:document ` + testNSW + `><w:body>` + body + `</w:body></w:document>`}
}

func xlsxParts(sheet1 string) map[string]string {
	return map[string]string{
		"xl/workbook.xml": `<workbook ` + testNSS + `><sheets><sheet name="Data" sheetId="1" r:id="rId1"/><sheet name="Other" sheetId="2" r:id="rId2"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships ` + testRelNS + `>` +
			`<Relationship Id="rId2" Type="x" Target="/xl/worksheets/sheet2.xml"/>` +
			`<Relationship Id="rId1" Type="x" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/sharedStrings.xml":     `<sst ` + testNSS + `><si><t>plain</t></si><si><r><t>rich </t></r><r><t>text</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet ` + testNSS + `><sheetData>` + sheet1 + `</sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": `<worksheet ` + testNSS + `><sheetData><row r="1"><c r="A1" t="str"><v>formula result</v></c></row></sheetData></worksheet>`,
	}
}

func slide(paras string) string {
	return `<p:sld ` + testNSP + `><p:cSld><p:spTree><p:sp><p:txBody>` + paras + `</p:txBody></p:sp></p:spTree></p:cSld></p:sld>`
}

func pptxParts() map[string]string {
	return map[string]string{
		"ppt/presentation.xml": `<p:presentation ` + testNSP + `><p:sldIdLst><p:sldId id="256" r:id="rId9"/><p:sldId id="257" r:id="rId3"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships ` + testRelNS + `>` +
			`<Relationship Id="rId3" Type="x" Target="slides/slide1.xml"/>` +
			`<Relationship Id="rId9" Type="x" Target="/ppt/slides/slide2.xml"/></Relationships>`,
		"ppt/slides/slide1.xml": slide(`<a:p><a:r><a:t>Second in order</a:t></a:r></a:p>`),
		"ppt/slides/slide2.xml": slide(`<a:p><a:r><a:t>Hello </a:t></a:r><a:r><a:t>deck</a:t></a:r></a:p><a:p></a:p><a:p><a:r><a:t>a</a:t></a:r><a:br/><a:r><a:t>b</a:t></a:r></a:p>`),
	}
}

func TestOfficeText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		ext   string
		parts map[string]string
		want  string
	}{
		{
			"docx paragraphs tab break and table", ".docx",
			docxParts(`<w:p><w:r><w:t>Title</w:t></w:r></w:p>` +
				`<w:p><w:r><w:t>a</w:t><w:tab/><w:t>b</w:t><w:br/><w:t>c</w:t></w:r></w:p>` +
				`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>r1c1</w:t></w:r></w:p><w:p><w:r><w:t>more</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>r1c2</w:t></w:r></w:p></w:tc></w:tr>` +
				`<w:tr><w:tc><w:p><w:r><w:t>r2c1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>r2c2</w:t></w:r></w:p></w:tc></w:tr></w:tbl>` +
				`<w:p><w:r><w:t>end  </w:t></w:r></w:p>`),
			"Title\na\tb\nc\nr1c1 more\tr1c2\nr2c1\tr2c2\nend",
		},
		{
			"docx other prefix", ".docx",
			map[string]string{"word/document.xml": `<x:document xmlns:x="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><x:body><x:p><x:r><x:t>hi</x:t></x:r></x:p></x:body></x:document>`},
			"hi",
		},
		{
			"xlsx sheets strings gaps and absolute target", ".xlsx",
			xlsxParts(`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row>` +
				`<row r="2"><c r="A2"><v>42</v></c><c r="B2" t="inlineStr"><is><t>inline</t></is></c><c r="C2" t="b"><v>1</v></c></row>` +
				`<row r="3"><c r="A3"/></row>` +
				`<row r="4"><c><v>x</v></c><c><v>y</v></c></row>`),
			"# sheet: Data\nplain\t\trich text\n42\tinline\tTRUE\nx\ty\n# sheet: Other\nformula result",
		},
		{
			"pptx follows sldIdLst order", ".pptx", pptxParts(),
			"# slide 1\nHello deck\na\nb\n# slide 2\nSecond in order",
		},
		{
			"pptx falls back to numeric slide order", ".pptx",
			map[string]string{
				"ppt/slides/slide10.xml": slide(`<a:p><a:r><a:t>ten</a:t></a:r></a:p>`),
				"ppt/slides/slide2.xml":  slide(`<a:p><a:r><a:t>two</a:t></a:r></a:p>`),
			},
			"# slide 1\ntwo\n# slide 2\nten",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := buildZip(t, tc.parts)
			got, err := officeText(bytes.NewReader(data), int64(len(data)), tc.ext)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOfficeTextErrors(t *testing.T) {
	t.Parallel()
	bomb := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("word/document.xml")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, `<w:document `+testNSW+`><w:body><w:p><w:r><w:t>`); err != nil {
			t.Fatal(err)
		}
		chunk := []byte(strings.Repeat("a", 1<<16))
		for i := 0; i < (65<<20)/len(chunk)+1; i++ {
			if _, err := w.Write(chunk); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}()
	notZip := []byte("this is not a zip")
	cases := []struct {
		name string
		ext  string
		data []byte
		want string
	}{
		{"not a zip", ".docx", notZip, "not a valid .docx file"},
		{"docx without document.xml", ".docx", buildZip(t, map[string]string{"other.xml": "<a/>"}), "word/document.xml"},
		{"xlsx without workbook", ".xlsx", buildZip(t, map[string]string{"other.xml": "<a/>"}), "xl/workbook.xml"},
		{"pptx without slides", ".pptx", buildZip(t, map[string]string{"other.xml": "<a/>"}), "no slides"},
		{"zip bomb", ".docx", bomb, "document expands to more than 64 MiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := officeText(bytes.NewReader(tc.data), int64(len(tc.data)), tc.ext)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestOfficeDocxThroughFileRead(t *testing.T) {
	f := newFixture(t)
	data := buildZip(t, docxParts(`<w:p><w:r><w:t>first</w:t></w:r></w:p><w:p><w:r><w:t>second</w:t></w:r></w:p>`))
	if err := os.WriteFile(filepath.Join(f.root, "memo.docx"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, f.root, "file.read", `{"path":"memo.docx"}`)
	if res.IsError || !res.Untrusted || !strings.Contains(res.Content, "1|first") || !strings.Contains(res.Content, "2|second") {
		t.Fatalf("result = %+v, want numbered untrusted text", res)
	}
}
