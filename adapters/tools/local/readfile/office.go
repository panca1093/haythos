package readfile

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

const (
	officeBudget = 64 << 20
	outputBudget = 16 << 20
	maxSheetCols = 16384

	nsWord = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	nsDraw = "http://schemas.openxmlformats.org/drawingml/2006/main"
	nsRel  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
)

var (
	errOfficeBomb  = errors.New("document expands to more than 64 MiB")
	errOutputBomb  = errors.New("document text is larger than 16 MiB")
	errMissingPart = errors.New("missing part")
	errBadXML      = errors.New("bad XML")
)

type officeDoc struct {
	parts   map[string]*zip.File
	left    int64
	outLeft int64
}

func (d *officeDoc) spend(n int) error {
	if d.outLeft -= int64(n); d.outLeft < 0 {
		return errOutputBomb
	}
	return nil
}

func badXML(err error) error {
	if errors.Is(err, errOfficeBomb) || errors.Is(err, errOutputBomb) {
		return err
	}
	return errBadXML
}

type budgetReader struct {
	r    io.Reader
	left *int64
}

func (b budgetReader) Read(p []byte) (int, error) {
	if int64(len(p)) > *b.left+1 {
		p = p[:*b.left+1]
	}
	n, err := b.r.Read(p)
	*b.left -= int64(n)
	if *b.left < 0 {
		return n, errOfficeBomb
	}
	return n, err
}

// officeText extracts the text of a docx, xlsx or pptx (ext includes the dot).
func officeText(r io.ReaderAt, size int64, ext string) (string, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return "", fmt.Errorf("not a valid %s file: %w", ext, err)
	}
	d := &officeDoc{parts: make(map[string]*zip.File, len(zr.File)), left: officeBudget, outLeft: outputBudget}
	for _, f := range zr.File {
		d.parts[f.Name] = f
	}
	var lines []string
	switch ext {
	case ".docx":
		lines, err = d.docx()
	case ".xlsx":
		lines, err = d.xlsx()
	case ".pptx":
		lines, err = d.pptx()
	default:
		err = fmt.Errorf("unsupported office format %s", ext)
	}
	if err != nil {
		switch {
		case errors.Is(err, errMissingPart), errors.Is(err, errBadXML):
			return "", fmt.Errorf("not a valid %s file (%w)", ext, err)
		}
		return "", err
	}
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' })
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n"), nil
}

func (d *officeDoc) decoder(name string) (*xml.Decoder, io.Closer, error) {
	f, ok := d.parts[name]
	if !ok {
		return nil, nil, errMissingPart
	}
	rc, err := f.Open()
	if err != nil {
		return nil, nil, errBadXML
	}
	return xml.NewDecoder(budgetReader{r: rc, left: &d.left}), rc, nil
}

func attr(se xml.StartElement, space, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local && a.Name.Space == space {
			return a.Value
		}
	}
	return ""
}

func resolvePart(dir, target string) string {
	if strings.HasPrefix(target, "/") {
		return strings.TrimPrefix(target, "/")
	}
	return path.Join(dir, target)
}

func (d *officeDoc) rels(part string) (map[string]string, error) {
	dec, rc, err := d.decoder(path.Join(path.Dir(part), "_rels", path.Base(part)+".rels"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	out := map[string]string{}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, badXML(err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "Relationship" {
			out[attr(se, "", "Id")] = attr(se, "", "Target")
		}
	}
}

// elemText reads the character data up to the end of the element whose start was just consumed.
func elemText(dec *xml.Decoder) (string, error) {
	var sb strings.Builder
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			sb.Write(t)
		}
	}
	return sb.String(), nil
}

// runText concatenates every t element under the element whose start was just consumed, skipping phonetic runs.
func runText(dec *xml.Decoder) (string, error) {
	var sb strings.Builder
	inT, skip := false, 0
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case t.Name.Local == "rPh":
				skip++
			case t.Name.Local == "t" && skip == 0:
				inT = true
			}
		case xml.EndElement:
			depth--
			switch t.Name.Local {
			case "rPh":
				skip--
			case "t":
				inT = false
			}
		case xml.CharData:
			if inT {
				sb.Write(t)
			}
		}
	}
	return sb.String(), nil
}

var cellSpaces = strings.NewReplacer("\n", " ", "\t", " ")

func (d *officeDoc) docx() ([]string, error) {
	dec, rc, err := d.decoder("word/document.xml")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var lines, cellParts, rowCells []string
	var para strings.Builder
	tcDepth, inT := 0, false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		if err != nil {
			return nil, badXML(err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != nsWord {
				continue
			}
			switch t.Name.Local {
			case "t":
				inT = true
			case "tab":
				para.WriteByte('\t')
			case "br", "cr":
				para.WriteByte('\n')
			case "tc":
				tcDepth++
			}
		case xml.EndElement:
			if t.Name.Space != nsWord {
				continue
			}
			switch t.Name.Local {
			case "t":
				inT = false
			case "p":
				text := para.String()
				para.Reset()
				if err := d.spend(len(text) + 1); err != nil {
					return nil, err
				}
				if tcDepth == 0 {
					lines = append(lines, text)
				} else if s := strings.TrimSpace(cellSpaces.Replace(text)); s != "" {
					cellParts = append(cellParts, s)
				}
			case "tc":
				if tcDepth--; tcDepth == 0 {
					rowCells = append(rowCells, strings.Join(cellParts, " "))
					cellParts = nil
				}
			case "tr":
				if tcDepth == 0 {
					lines = append(lines, strings.Join(rowCells, "\t"))
					rowCells = nil
				}
			}
		case xml.CharData:
			if inT {
				para.Write(t)
			}
		}
	}
}

func (d *officeDoc) sharedStrings() ([]string, error) {
	if _, ok := d.parts["xl/sharedStrings.xml"]; !ok {
		return nil, nil
	}
	dec, rc, err := d.decoder("xl/sharedStrings.xml")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var out []string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, badXML(err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "si" {
			s, err := runText(dec)
			if err != nil {
				return nil, badXML(err)
			}
			out = append(out, s)
		}
	}
}

func (d *officeDoc) xlsx() ([]string, error) {
	dec, rc, err := d.decoder("xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	type sheet struct{ name, rid string }
	var sheets []sheet
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, badXML(err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sheet" {
			sheets = append(sheets, sheet{attr(se, "", "name"), attr(se, nsRel, "id")})
		}
	}
	rels, err := d.rels("xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	sst, err := d.sharedStrings()
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, s := range sheets {
		target, ok := rels[s.rid]
		if !ok {
			return nil, errMissingPart
		}
		rows, err := d.sheetRows(resolvePart("xl", target), sst)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "# sheet: "+s.name)
		lines = append(lines, rows...)
	}
	return lines, nil
}

func colIndex(ref string) int {
	col := 0
	for _, c := range ref {
		if c < 'A' || c > 'Z' {
			break
		}
		col = col*26 + int(c-'A') + 1
		if col > maxSheetCols {
			return maxSheetCols
		}
	}
	return col - 1
}

func (d *officeDoc) sheetRows(part string, sst []string) ([]string, error) {
	dec, rc, err := d.decoder(part)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var rows, fields []string
	var typ, val string
	col, nextCol := 0, 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, badXML(err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				fields, nextCol = nil, 0
			case "c":
				col = nextCol
				if c := colIndex(attr(t, "", "r")); c >= 0 {
					col = c
				}
				nextCol = col + 1
				typ, val = attr(t, "", "t"), ""
			case "v":
				val, err = elemText(dec)
			case "is":
				val, err = runText(dec)
			}
			if err != nil {
				return nil, badXML(err)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "c":
				switch typ {
				case "s":
					if i, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && i >= 0 && i < len(sst) {
						val = sst[i]
					}
				case "b":
					val = map[string]string{"1": "TRUE", "0": "FALSE"}[strings.TrimSpace(val)]
				}
				if val != "" && col >= 0 && col < maxSheetCols {
					if err := d.spend(len(val) + 1); err != nil {
						return nil, err
					}
					for len(fields) <= col {
						fields = append(fields, "")
					}
					fields[col] = cellSpaces.Replace(val)
				}
			case "row":
				if len(fields) > 0 {
					if err := d.spend(len(fields)); err != nil {
						return nil, err
					}
					rows = append(rows, strings.Join(fields, "\t"))
				}
			}
		}
	}
}

func (d *officeDoc) slideParts() []string {
	if dec, rc, err := d.decoder("ppt/presentation.xml"); err == nil {
		defer func() { _ = rc.Close() }()
		if rels, err := d.rels("ppt/presentation.xml"); err == nil {
			var out []string
			for {
				tok, err := dec.Token()
				if err != nil {
					break
				}
				if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sldId" {
					if target, ok := rels[attr(se, nsRel, "id")]; ok {
						out = append(out, resolvePart("ppt", target))
					}
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	type numbered struct {
		name string
		n    int
	}
	var found []numbered
	for name := range d.parts {
		if rest, ok := strings.CutPrefix(name, "ppt/slides/slide"); ok {
			if n, err := strconv.Atoi(strings.TrimSuffix(rest, ".xml")); err == nil && strings.HasSuffix(rest, ".xml") {
				found = append(found, numbered{name, n})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].n < found[j].n })
	out := make([]string, len(found))
	for i, f := range found {
		out[i] = f.name
	}
	return out
}

func (d *officeDoc) pptx() ([]string, error) {
	slides := d.slideParts()
	if len(slides) == 0 {
		return nil, errors.New("presentation has no slides")
	}
	var lines []string
	for i, part := range slides {
		dec, rc, err := d.decoder(part)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		lines = append(lines, fmt.Sprintf("# slide %d", i+1))
		var para strings.Builder
		inT := false
		for {
			tok, err := dec.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, badXML(err)
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Space != nsDraw {
					continue
				}
				switch t.Name.Local {
				case "t":
					inT = true
				case "br":
					para.WriteByte('\n')
				}
			case xml.EndElement:
				if t.Name.Space != nsDraw {
					continue
				}
				switch t.Name.Local {
				case "t":
					inT = false
				case "p":
					if strings.TrimSpace(para.String()) != "" {
						if err := d.spend(para.Len() + 1); err != nil {
							return nil, err
						}
						lines = append(lines, para.String())
					}
					para.Reset()
				}
			case xml.CharData:
				if inT {
					para.Write(t)
				}
			}
		}
	}
	return lines, nil
}
