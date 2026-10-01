// Package filestest is a fake Slack for the tests of the file route: files.info
// for the files it holds and their bytes under /files-pri/, the way the real
// one answers both.
package filestest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
