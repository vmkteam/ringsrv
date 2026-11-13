// Package publictest is a fake Mattermost API v4 for the tests of the
// public-only check and of the Mattermost file route: channels with their
// types, canned answers by method and path, files with their info and bytes —
// optionally under a mount path, the way a plugin repeats the API.
package publictest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Mattermost serves what was added to it and counts what was asked.
type Mattermost struct {
	srv *httptest.Server
	// Prefix, when set, is the mount path every request must carry; it is
	// taken off before the path is read.
	Prefix string

	mu        sync.Mutex
	channels  map[string]string
	answers   map[string]string
	files     map[string]File
	requests  []string
	lookups   atomic.Int32
	downloads atomic.Int32
}

// File is one file: what its info says and what its download answers.
type File struct {
	Info        map[string]any
	Body        []byte
	ContentType string
}

// New starts the fake for the length of the test.
func New(t testing.TB) *Mattermost {
	t.Helper()
	m := &Mattermost{channels: map[string]string{}, answers: map[string]string{}, files: map[string]File{}}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

// ID makes a valid Mattermost id out of a short name: the name, padded with
// x to 26 characters.
func ID(name string) string {
	return name + strings.Repeat("x", 26-len(name))
}

// URL is the fake's base URL, the profile's BaseURL.
func (m *Mattermost) URL() string { return m.srv.URL }

// Channel adds a channel of a type: O public, P private, D direct, G group.
// A type that is a status code — "403", "500" — makes its lookup answer
// with that status.
func (m *Mattermost) Channel(id, typ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channels[id] = typ
}

// Answer sets what method and path answer, with status 200.
func (m *Mattermost) Answer(method, path, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answers[method+" "+path] = body
}

// AddFile adds a file posted in a channel; edit changes its info after.
func (m *Mattermost) AddFile(id, channel, name, mimetype string, body []byte, edit func(map[string]any)) {
	ext := ""
	if i := strings.LastIndex(name, "."); i >= 0 {
		ext = name[i+1:]
	}
	info := map[string]any{
		"id": id, "channel_id": channel, "post_id": ID("post"), "user_id": ID("user"), "name": name,
		"extension": ext, "size": len(body), "mime_type": mimetype, "delete_at": 0, "archived": false,
	}
	if edit != nil {
		edit(info)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[id] = File{Info: info, Body: body, ContentType: mimetype}
}

// Lookups is how many channels were looked up by id.
func (m *Mattermost) Lookups() int { return int(m.lookups.Load()) }

// Downloads is how many file downloads were asked for.
func (m *Mattermost) Downloads() int { return int(m.downloads.Load()) }

// Requests are the method and path of every request, in order.
func (m *Mattermost) Requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.requests...)
}

func (m *Mattermost) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if m.Prefix != "" {
		rest, ok := strings.CutPrefix(path, m.Prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		path = rest
	}
	m.mu.Lock()
	m.requests = append(m.requests, r.Method+" "+path)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		if id, ok := strings.CutPrefix(path, "/api/v4/channels/"); ok && !strings.Contains(id, "/") {
			m.serveChannel(w, id)
			return
		}
		if rest, ok := strings.CutPrefix(path, "/api/v4/files/"); ok {
			m.serveFile(w, rest)
			return
		}
	}
	m.mu.Lock()
	body, ok := m.answers[r.Method+" "+path]
	m.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	_, _ = w.Write([]byte(body))
}

// serveChannel answers a lookup of a channel by id.
func (m *Mattermost) serveChannel(w http.ResponseWriter, id string) {
	m.lookups.Add(1)
	m.mu.Lock()
	typ, known := m.channels[id]
	m.mu.Unlock()
	switch {
	case !known:
		notFound(w)
	case typ == "403" || typ == "500":
		code := http.StatusForbidden
		if typ == "500" {
			code = http.StatusInternalServerError
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"id":"api.context.permissions.app_error","status_code":` + typ + `}`))
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "type": typ, "name": "channel-" + id[:4], "team_id": ID("team")})
	}
}

// serveFile answers a file's info or its bytes.
func (m *Mattermost) serveFile(w http.ResponseWriter, rest string) {
	id, sub, _ := strings.Cut(rest, "/")
	m.mu.Lock()
	f, known := m.files[id]
	m.mu.Unlock()
	switch {
	case !known:
		notFound(w)
	case sub == "info":
		_ = json.NewEncoder(w).Encode(f.Info)
	case sub == "":
		m.downloads.Add(1)
		w.Header().Set("Content-Type", f.ContentType)
		_, _ = w.Write(f.Body)
	default:
		notFound(w)
	}
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"id":"app.not_found","message":"not found","status_code":404}`))
}
