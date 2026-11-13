package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
	"time"
)

// Limits of reading a workbook. An xlsx is a zip of XML, and the 10 MB the
// route downloads can unpack to hundreds: what is read out of it is bounded on
// its own.
const (
	// maxSheetXML is how much of a sheet's XML is read. It is streamed and
	// never held whole.
	maxSheetXML = 192 << 20
	// maxPartBytes is the most of any other part — the workbook, its
	// relationships, the styles, the shared strings — held in memory.
	maxPartBytes = 32 << 20
	// maxSheetText is the most text one sheet may turn into.
	maxSheetText = 32 << 20
	// maxColumns and maxRows are the size of a sheet in Excel: XFD1048576.
	maxColumns = 16384
	maxRows    = 1 << 20
)

// elemPhonetic is a phonetic guide inside a string, which is not its text.
const elemPhonetic = "rPh"

// errWorkbookTooLarge is a workbook past one of the limits.
var errWorkbookTooLarge = errors.New("too large")

// sheet answers a workbook with the lines of one sheet: a line per row, the
// line's number the row's, cells by tab. The lines go through the same window
// and grep as a text file's, and the first row is the header when a table is
// read from the middle.
//
// Reading takes the instance's decode slot, as a picture does: it is the other
// thing this route does that takes tens of megabytes.
func (f *Fetcher) sheet(ctx context.Context, src source, p params, budget int) (*Text, error) {
	select {
	case f.decodes <- struct{}{}:
		defer func() { <-f.decodes }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	id := src.meta.ID
	if src.meta.Entry != "" {
		id += " " + src.meta.Entry
	}
	wb, err := openWorkbook(src.data)
	if err != nil {
		return nil, workbookError(id, err)
	}
	ref, err := wb.pick(p.Sheet)
	if err != nil {
		return nil, err
	}
	text, err := wb.text(ref)
	if err != nil {
		return nil, workbookError(id, err)
	}
	out := &Text{Meta: src.meta, Sheet: ref.name, Sheets: wb.names()}
	out.fill(text, p, budget, true)
	return out, nil
}

func workbookError(id string, err error) error {
	if errors.Is(err, errWorkbookTooLarge) {
		return refused(CodeTooLarge, "workbook %s is too large to read: %s", id, err)
	}
	return refused(CodeUnsupported, "file %s is not an xlsx workbook this server can read: %s", id, err)
}

// workbook is an opened xlsx: its worksheets, the strings they share and which
// cell styles are dates.
type workbook struct {
	parts    map[string]*zip.File
	sheets   []sheetRef
	strings  []string
	dates    []bool
	date1904 bool
	// sharedPart and stylesPart are found through the workbook's
	// relationships; either may be absent.
	sharedPart, stylesPart string
}

// sheetRef is a worksheet: its name in the workbook and its part in the zip.
type sheetRef struct {
	name, part string
	hidden     bool
}

// Relationship types a workbook is read through, by their last segment: the
// transitional and the strict schemas differ only before it.
const (
	relOfficeDocument = "/officeDocument"
	relWorksheet      = "/worksheet"
	relSharedStrings  = "/sharedStrings"
	relStyles         = "/styles"
)

func openWorkbook(data []byte) (*workbook, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	wb := &workbook{parts: make(map[string]*zip.File, len(zr.File))}
	for _, zf := range zr.File {
		// Part names are not case-sensitive in the packaging format.
		wb.parts[strings.ToLower(zf.Name)] = zf
	}

	main := "xl/workbook.xml"
	if pkg, relErr := wb.rels("_rels/.rels", ""); relErr == nil {
		if target, ok := pkg.first(relOfficeDocument); ok {
			main = target
		}
	}
	var doc xWorkbook
	if err = wb.decode(main, &doc); err != nil {
		return nil, fmt.Errorf("the workbook part: %w", err)
	}
	wb.date1904 = doc.Pr.Date1904 == "1" || doc.Pr.Date1904 == "true"

	rels, err := wb.rels(path.Join(path.Dir(main), "_rels", path.Base(main)+".rels"), path.Dir(main))
	if err != nil {
		return nil, fmt.Errorf("the workbook's relationships: %w", err)
	}
	for _, s := range doc.Sheets {
		if target, ok := rels.byID(s.rid(), relWorksheet); ok {
			wb.sheets = append(wb.sheets, sheetRef{name: s.Name, part: target, hidden: s.State != "" && s.State != "visible"})
		}
	}
	if len(wb.sheets) == 0 {
		return nil, errors.New("it has no worksheets")
	}
	wb.sharedPart, _ = rels.first(relSharedStrings)
	wb.stylesPart, _ = rels.first(relStyles)
	return wb, nil
}

// pick finds the sheet asked for: by name, then by name without case, then by
// number from 1. None asked for is the first one that is not hidden.
func (wb *workbook) pick(want string) (sheetRef, error) {
	if want == "" {
		for _, s := range wb.sheets {
			if !s.hidden {
				return s, nil
			}
		}
		return wb.sheets[0], nil
	}
	for _, s := range wb.sheets {
		if s.name == want {
			return s, nil
		}
	}
	for _, s := range wb.sheets {
		if strings.EqualFold(s.name, want) {
			return s, nil
		}
	}
	if n, err := strconv.Atoi(want); err == nil && n >= 1 && n <= len(wb.sheets) {
		return wb.sheets[n-1], nil
	}
	return sheetRef{}, fmt.Errorf("%w: the workbook has no sheet %q — its sheets: %s", ErrBadParams, want, strings.Join(wb.names(), ", "))
}

func (wb *workbook) names() []string {
	out := make([]string, len(wb.sheets))
	for i, s := range wb.sheets {
		out[i] = s.name
	}
	return out
}

// text turns a sheet into lines. Rows the sheet skips become empty lines, so
// line N is row N; a cell's place in its row comes from its reference, so a
// column stays a column when the cells before it are empty. Empty cells at
// the end of a row are dropped.
func (wb *workbook) text(ref sheetRef) ([]byte, error) {
	if err := wb.loadStrings(); err != nil {
		return nil, fmt.Errorf("the shared strings: %w", err)
	}
	if err := wb.loadStyles(); err != nil {
		return nil, fmt.Errorf("the styles: %w", err)
	}
	zf, ok := wb.parts[strings.ToLower(ref.part)]
	if !ok {
		return nil, fmt.Errorf("sheet %q has no part %s", ref.name, ref.part)
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	lr := &capReader{r: rc, left: maxSheetXML}
	d := xml.NewDecoder(lr)

	var w rowWriter
	for {
		tok, err := d.RawToken()
		switch {
		case lr.over:
			return nil, fmt.Errorf("%w: the sheet's XML is over %d MB unpacked", errWorkbookTooLarge, maxSheetXML>>20)
		case errors.Is(err, io.EOF):
			return w.out.Bytes(), nil
		case err != nil:
			return nil, err
		}
		done, err := wb.step(d, tok, &w)
		switch {
		case err != nil:
			return nil, err
		case done:
			return w.out.Bytes(), nil
		case w.out.Len() > maxSheetText:
			return nil, fmt.Errorf("%w: the sheet is over %d MB as text", errWorkbookTooLarge, maxSheetText>>20)
		}
	}
}

// rowWriter collects a sheet's text: the rows written so far and the cells of
// the row being read.
type rowWriter struct {
	out   bytes.Buffer
	cells []string
	rows  int
}

// step handles one token of a sheet; done is the end of its data.
func (wb *workbook) step(d *xml.Decoder, tok xml.Token, w *rowWriter) (done bool, err error) {
	switch t := tok.(type) {
	case xml.StartElement:
		switch t.Name.Local {
		case "row":
			return false, w.startRow(attr(t, "r"))
		case "c":
			col, value, err := wb.cell(d, t, len(w.cells))
			if err != nil {
				return false, err
			}
			w.set(col, value)
		}
	case xml.EndElement:
		switch t.Name.Local {
		case "row":
			w.endRow()
		case "sheetData":
			return true, nil
		}
	}
	return false, nil
}

// startRow begins row number r: the rows the sheet skipped before it become
// empty lines. A row with no number, or one out of order, is the next line.
func (w *rowWriter) startRow(r string) error {
	n, _ := strconv.Atoi(r)
	if n <= w.rows {
		n = w.rows + 1
	}
	if n > maxRows {
		return fmt.Errorf("row %d is past the last row of a sheet", n)
	}
	for ; w.rows < n-1; w.rows++ {
		w.out.WriteByte('\n')
	}
	w.cells = w.cells[:0]
	return nil
}

func (w *rowWriter) set(col int, value string) {
	for len(w.cells) <= col {
		w.cells = append(w.cells, "")
	}
	w.cells[col] = value
}

// endRow writes the row as a line, empty cells at its end dropped.
func (w *rowWriter) endRow() {
	last := len(w.cells)
	for last > 0 && w.cells[last-1] == "" {
		last--
	}
	for i, c := range w.cells[:last] {
		if i > 0 {
			w.out.WriteByte('\t')
		}
		w.out.WriteString(c)
	}
	w.out.WriteByte('\n')
	w.rows++
}

// cell reads one <c> to its end: the column from its reference — next when it
// has none — and the value as its type says. A formula's cached result is the
// value; the formula is not shown.
func (wb *workbook) cell(d *xml.Decoder, start xml.StartElement, next int) (int, string, error) {
	col, ok := column(attr(start, "r"))
	if !ok {
		col = next
	}
	if col >= maxColumns {
		return 0, "", fmt.Errorf("cell %s is past the last column of a sheet", attr(start, "r"))
	}
	typ := attr(start, "t")
	style, _ := strconv.Atoi(attr(start, "s"))

	var v, inline strings.Builder
	var inV, inT, inPhonetic bool
	for {
		tok, err := d.RawToken()
		if err != nil {
			return 0, "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "v":
				inV = true
			case "t":
				inT = true
			case elemPhonetic:
				inPhonetic = true
			}
		case xml.CharData:
			switch {
			case inV:
				v.Write(t)
			case inT && !inPhonetic:
				inline.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "t":
				inT = false
			case elemPhonetic:
				inPhonetic = false
			case "c":
				value, err := wb.value(typ, style, v.String(), inline.String())
				return col, oneLine(value), err
			}
		}
	}
}

// value is a cell's text by its type: a shared string, an inline one, a
// boolean, an error or a date as written, and a number — a date when its style
// formats it as one.
func (wb *workbook) value(typ string, style int, v, inline string) (string, error) {
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || i < 0 || i >= len(wb.strings) {
			return "", fmt.Errorf("shared string %q is not in the table of %d", v, len(wb.strings))
		}
		return wb.strings[i], nil
	case "inlineStr":
		return inline, nil
	case "b":
		if strings.TrimSpace(v) == "1" {
			return "TRUE", nil
		}
		return "FALSE", nil
	case "str", "e", "d":
		return v, nil
	}
	if style >= 0 && style < len(wb.dates) && wb.dates[style] {
		return serialDate(v, wb.date1904), nil
	}
	return v, nil
}

// loadStrings reads the table of strings the cells share, the text of each
// item with its rich-text runs joined and its phonetic guide left out.
func (wb *workbook) loadStrings() error {
	if wb.sharedPart == "" {
		return nil
	}
	data, err := wb.read(wb.sharedPart)
	if errors.Is(err, errNoPart) {
		return nil
	}
	if err != nil {
		return err
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	var (
		cur                     strings.Builder
		inItem, inT, inPhonetic bool
	)
	for {
		tok, err := d.RawToken()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inItem = true
				cur.Reset()
			case "t":
				inT = true
			case elemPhonetic:
				inPhonetic = true
			}
		case xml.CharData:
			if inItem && inT && !inPhonetic {
				cur.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				inItem = false
				wb.strings = append(wb.strings, cur.String())
			case "t":
				inT = false
			case elemPhonetic:
				inPhonetic = false
			}
		}
	}
}

// loadStyles marks the cell styles whose number format is a date or a time.
func (wb *workbook) loadStyles() error {
	if wb.stylesPart == "" {
		return nil
	}
	var doc xStyles
	err := wb.decode(wb.stylesPart, &doc)
	if errors.Is(err, errNoPart) {
		return nil
	}
	if err != nil {
		return err
	}
	custom := make(map[int]string, len(doc.NumFmts))
	for _, nf := range doc.NumFmts {
		custom[nf.ID] = nf.Code
	}
	wb.dates = make([]bool, len(doc.Xfs))
	for i, xf := range doc.Xfs {
		if code, ok := custom[xf.NumFmtID]; ok {
			wb.dates[i] = isDateFormat(code)
		} else {
			wb.dates[i] = builtinDate(xf.NumFmtID)
		}
	}
	return nil
}

// builtinDate says a built-in number format is a date or a time: 14–22, and
// 27–36, 45–47, 50–58 of the East Asian locales.
func builtinDate(id int) bool {
	return id >= 14 && id <= 22 || id >= 27 && id <= 36 || id >= 45 && id <= 47 || id >= 50 && id <= 58
}

// isDateFormat says a number format code shows a date or a time: it has a
// y, m, d, h or s outside quoted text, escapes and brackets — [Red] and
// [$-419] are not dates, but [h] and [mm] of elapsed time are.
func isDateFormat(code string) bool {
	inQuote, inBracket, skip := false, false, false
	var bracket strings.Builder
	for _, r := range code {
		switch {
		case skip:
			skip = false
		case inQuote:
			inQuote = r != '"'
		case inBracket:
			if r == ']' {
				inBracket = false
				if b := strings.ToLower(bracket.String()); strings.Trim(b, "hms") == "" && b != "" {
					return true
				}
				continue
			}
			bracket.WriteRune(r)
		case r == '"':
			inQuote = true
		case r == '[':
			inBracket = true
			bracket.Reset()
		case r == '\\', r == '_', r == '*':
			skip = true
		case strings.ContainsRune("ymdhsYMDHS", r):
			return true
		}
	}
	return false
}

// serialDate writes a date serial as ISO 8601: a date alone, a time alone
// below one day, or both. The 1900 system counts from 1899-12-30 — which also
// absorbs the leap day 1900 never had — the 1904 one from 1904-01-01. What is
// not a number stays as it was.
func serialDate(v string, date1904 bool) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 || f > 2958466 { // 9999-12-31
		return v
	}
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		base = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	days := math.Floor(f)
	secs := math.Round((f - days) * 86400)
	t := base.AddDate(0, 0, int(days)).Add(time.Duration(secs) * time.Second)
	switch {
	case secs == 0:
		return t.Format("2006-01-02")
	case days == 0 && !date1904:
		return t.Format("15:04:05")
	}
	return t.Format("2006-01-02 15:04:05")
}

// column is the index from 0 of a cell reference's column: A is 0, AA is 26.
func column(ref string) (int, bool) {
	col, n := 0, 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		col = col*26 + int(r-'A'+1)
		n++
		if col > maxColumns {
			return maxColumns, true
		}
	}
	if n == 0 {
		return 0, false
	}
	return col - 1, true
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name && a.Name.Space == "" {
			return a.Value
		}
	}
	return ""
}

var errNoPart = errors.New("no such part")

// read takes a part out of the zip whole, up to maxPartBytes.
func (wb *workbook) read(name string) ([]byte, error) {
	zf, ok := wb.parts[strings.ToLower(name)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errNoPart, name)
	}
	data, err := unpack(zf, maxPartBytes)
	if errors.Is(err, errUnpackedTooLarge) {
		return nil, fmt.Errorf("%w: %s is over %d MB unpacked", errWorkbookTooLarge, name, maxPartBytes>>20)
	}
	return data, err
}

func (wb *workbook) decode(name string, v any) error {
	data, err := wb.read(name)
	if err != nil {
		return err
	}
	return xml.Unmarshal(data, v)
}

// rels reads a relationships part; targets are resolved against dir, the
// folder of the part they belong to.
func (wb *workbook) rels(name, dir string) (relations, error) {
	var doc xRels
	if err := wb.decode(name, &doc); err != nil {
		return nil, err
	}
	out := make(relations, 0, len(doc.Rels))
	for _, r := range doc.Rels {
		if r.Mode == "External" {
			continue
		}
		target := r.Target
		if strings.HasPrefix(target, "/") {
			target = strings.TrimPrefix(target, "/")
		} else {
			target = path.Join(dir, target)
		}
		out = append(out, relation{id: r.ID, typ: r.Type, target: target})
	}
	return out, nil
}

type relation struct{ id, typ, target string }

type relations []relation

func (rs relations) first(typ string) (string, bool) {
	for _, r := range rs {
		if strings.HasSuffix(r.typ, typ) {
			return r.target, true
		}
	}
	return "", false
}

func (rs relations) byID(id, typ string) (string, bool) {
	for _, r := range rs {
		if r.id == id && strings.HasSuffix(r.typ, typ) {
			return r.target, true
		}
	}
	return "", false
}

// The parts of a workbook read whole, as far as this route needs them.
type (
	xWorkbook struct {
		Pr struct {
			Date1904 string `xml:"date1904,attr"`
		} `xml:"workbookPr"`
		Sheets []xSheet `xml:"sheets>sheet"`
	}
	xSheet struct {
		Name  string     `xml:"name,attr"`
		State string     `xml:"state,attr"`
		Attrs []xml.Attr `xml:",any,attr"`
	}
	xRels struct {
		Rels []struct {
			ID     string `xml:"Id,attr"`
			Type   string `xml:"Type,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	xStyles struct {
		NumFmts []struct {
			ID   int    `xml:"numFmtId,attr"`
			Code string `xml:"formatCode,attr"`
		} `xml:"numFmts>numFmt"`
		Xfs []struct {
			NumFmtID int `xml:"numFmtId,attr"`
		} `xml:"cellXfs>xf"`
	}
)

// rid is the sheet's relationship id, r:id — whatever the prefix and the
// schema's namespace.
func (s xSheet) rid() string {
	for _, a := range s.Attrs {
		if a.Name.Local == "id" && a.Name.Space != "" {
			return a.Value
		}
	}
	return ""
}

// capReader reads up to left bytes and then reports over instead of reading
// on.
type capReader struct {
	r    io.Reader
	left int64
	over bool
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		c.over = true
		return 0, io.EOF
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
