// Package filestest is a fake Slack for the tests of the file route: files.info
// for the files it holds and their bytes under /files-pri/, the way the real
// one answers both. It also builds the archives and workbooks those files can
// be.
package filestest

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
)

// Slack serves the files added to it and counts the downloads.
type Slack struct {
	srv       *httptest.Server
	files     map[string]File
	downloads atomic.Int32
	auth      atomic.Value
}

// File is one file of the fake: what files.info says about it and what its
// download answers.
type File struct {
	Info        map[string]any
	Body        []byte
	ContentType string
	// Status, a 3xx, turns the download into a redirect to the sign-in page.
	Status int
	// Gzip compresses the download for a client that accepts it, as a CDN
	// does: what files.info calls the size is then the size unpacked.
	Gzip bool
}

// NewSlack starts the fake for the length of the test.
func NewSlack(t testing.TB) *Slack {
	t.Helper()
	s := &Slack{files: map[string]File{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Slack) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/files.info":
		f, ok := s.files[r.URL.Query().Get("file")]
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			_, _ = w.Write([]byte(`{"ok":false,"error":"file_not_found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "file": f.Info})
	case strings.HasPrefix(r.URL.Path, "/files-pri/"):
		s.downloads.Add(1)
		s.auth.Store(r.Header.Get("Authorization"))
		id := strings.TrimPrefix(r.URL.Path, "/files-pri/T0-")
		id, _, _ = strings.Cut(id, "/")
		f := s.files[id]
		if f.Status >= 300 && f.Status < 400 {
			http.Redirect(w, r, "/signin", f.Status)
			return
		}
		w.Header().Set("Content-Type", f.ContentType)
		if f.Gzip && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			_, _ = zw.Write(f.Body)
			_ = zw.Close()
			return
		}
		_, _ = w.Write(f.Body)
	default:
		http.NotFound(w, r)
	}
}

// URL is the fake's base URL, the profile's BaseURL.
func (s *Slack) URL() string { return s.srv.URL }

// Add registers a public file in one channel; edit changes what files.info
// says about it after.
func (s *Slack) Add(id, name, filetype, mimetype, mode string, body []byte, edit func(info map[string]any)) {
	info := map[string]any{
		"id": id, "name": name, "filetype": filetype, "mimetype": mimetype, "mode": mode,
		"size": len(body), "is_public": true, "is_external": false,
		"url_private_download": s.srv.URL + "/files-pri/T0-" + id + "/" + name,
		"channels":             []string{"C0123456789"},
	}
	if edit != nil {
		edit(info)
	}
	s.files[id] = File{Info: info, Body: body, ContentType: mimetype}
}

// Edit changes a file added before.
func (s *Slack) Edit(id string, edit func(*File)) {
	f := s.files[id]
	edit(&f)
	s.files[id] = f
}

// Downloads is how many downloads were asked for.
func (s *Slack) Downloads() int { return int(s.downloads.Load()) }

// Auth is the Authorization header of the last download.
func (s *Slack) Auth() string {
	v, _ := s.auth.Load().(string)
	return v
}

// PNG is a w×h picture of one colour.
func PNG(t testing.TB, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Entry is a file of an archive built by Zip.
type Entry struct {
	Name string
	Body []byte
	// CP866 writes the name the way the archiver built into Windows does:
	// in CP866, without the flag that marks a name as UTF-8.
	CP866 bool
}

// Zip is an archive of the entries, in their order; a name ending in / is a
// folder.
func Zip(t testing.TB, entries ...Entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		fh := &zip.FileHeader{Name: e.Name, Method: zip.Deflate, Modified: time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)}
		if e.CP866 {
			name, err := charmap.CodePage866.NewEncoder().String(e.Name)
			if err != nil {
				t.Fatal(err)
			}
			fh.Name, fh.NonUTF8 = name, true
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.Body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Workbook describes an xlsx for XLSX: its sheets in order, the shared
// strings their cells point at, and the styles — the number format of each
// cell style, and the custom formats by id.
type Workbook struct {
	Sheets   []Sheet
	Shared   []string
	Styles   []int
	Formats  map[int]string
	Date1904 bool
}

// Sheet is one worksheet: its name and the XML inside its <sheetData>.
type Sheet struct {
	Name   string
	Rows   string
	Hidden bool
}

// XLSX builds the workbook as Excel lays it out: the package relationships,
// the workbook and its relationships, the shared strings, the styles and a
// part per sheet.
func XLSX(t testing.TB, wb Workbook) []byte {
	t.Helper()
	const (
		ns     = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"`
		nsRel  = `xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
		relDoc = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"
	)
	var sheets, rels strings.Builder
	parts := []Entry{}
	for i, s := range wb.Sheets {
		state := ""
		if s.Hidden {
			state = ` state="hidden"`
		}
		fmt.Fprintf(&sheets, `<sheet name="%s" sheetId="%d"%s r:id="rId%d"/>`, xmlText(s.Name), i+1, state, i+1)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="%sworksheet" Target="worksheets/sheet%d.xml"/>`, i+1, relDoc, i+1)
		parts = append(parts, Entry{Name: fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1),
			Body: []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet ` + ns + `><sheetData>` + s.Rows + `</sheetData></worksheet>`)})
	}
	fmt.Fprintf(&rels, `<Relationship Id="rIdS" Type="%ssharedStrings" Target="sharedStrings.xml"/>`, relDoc)
	fmt.Fprintf(&rels, `<Relationship Id="rIdY" Type="%sstyles" Target="/xl/styles.xml"/>`, relDoc)

	var shared strings.Builder
	for _, s := range wb.Shared {
		fmt.Fprintf(&shared, `<si><t xml:space="preserve">%s</t></si>`, xmlText(s))
	}
	var formats, xfs strings.Builder
	for id, code := range wb.Formats {
		fmt.Fprintf(&formats, `<numFmt numFmtId="%d" formatCode="%s"/>`, id, xmlText(code))
	}
	for _, id := range wb.Styles {
		fmt.Fprintf(&xfs, `<xf numFmtId="%d" fontId="0" fillId="0" borderId="0"/>`, id)
	}
	date1904 := ""
	if wb.Date1904 {
		date1904 = ` date1904="1"`
	}
	parts = append([]Entry{
		{Name: "[Content_Types].xml", Body: []byte(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`)},
		{Name: "_rels/.rels", Body: []byte(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="` + relDoc + `officeDocument" Target="xl/workbook.xml"/></Relationships>`)},
		{Name: "xl/workbook.xml", Body: []byte(`<workbook ` + ns + ` ` + nsRel + `><workbookPr` + date1904 + `/><sheets>` + sheets.String() + `</sheets></workbook>`)},
		{Name: "xl/_rels/workbook.xml.rels", Body: []byte(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + rels.String() + `</Relationships>`)},
		{Name: "xl/sharedStrings.xml", Body: []byte(`<sst ` + ns + `>` + shared.String() + `</sst>`)},
		{Name: "xl/styles.xml", Body: []byte(`<styleSheet ` + ns + `><numFmts>` + formats.String() + `</numFmts><cellXfs>` + xfs.String() + `</cellXfs></styleSheet>`)},
	}, parts...)
	return Zip(t, parts...)
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
