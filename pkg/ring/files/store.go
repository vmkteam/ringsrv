package files

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vmkteam/ringsrv/pkg/client/sftpfs"
	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/pkg/sftp"
)

// A store is a directory tree the model reads by path: a WebDAV collection or
// a directory on an SFTP server, under the profile's Root. /files/list shows a
// directory the way an archive's table of contents is shown, /files/read a
// file the way the file route shows one — text, picture, workbook, archive.
//
// Nothing in a store is followed out of it: a path is clean and below Root
// before it leaves, and on SFTP a symlink anywhere on it is refused rather
// than resolved. The account's own permissions on the server are the fence
// that stands when this one does not: it should be read-only.

// Limits of a store.
const (
	// maxStorePath is the longest path a call may name.
	maxStorePath = 1024
	// davListMaxBytes caps a WebDAV directory listing: a few hundred bytes
	// an entry.
	davListMaxBytes = 16 << 20
	// sftpDialTimeout bounds the connection and the handshake.
	sftpDialTimeout = 15 * time.Second
)

// storeEntry is a file or a directory as the store describes it.
type storeEntry struct {
	name     string
	dir      bool
	link     bool
	size     int64
	modified time.Time
	mimetype string
}

// store is one session with a store. Paths are full: Root included.
type store interface {
	list(ctx context.Context, dir string) ([]storeEntry, error)
	stat(ctx context.Context, file string) (storeEntry, error)
	read(ctx context.Context, file string) ([]byte, error)
	// follows says the store has links a path could leave Root through.
	follows() bool
	close()
}

// Store serves the two routes of a store profile; route is target.FilesListPath
// or target.FilesReadPath.
func (f *Fetcher) Store(ctx context.Context, prof *target.Profile, route, rawPath string, budget int) (*Result, error) {
	read := route == target.FilesReadPath
	p, err := parseStoreParams(rawPath, read)
	if err != nil {
		return nil, err
	}
	// The HTTP layer refuses ".." anywhere in a path, a part of a name
	// included: say so here rather than fail as a broken path there.
	if prof.Store == target.StoreWebDAV && strings.Contains(p.Path, "..") {
		return nil, refused(CodeUnsupported, "%s: a name with .. in it cannot be read over WebDAV by this server", p.Path)
	}
	// An SFTP session goes around the HTTP client, so it takes the client's
	// slot and its timeout itself: one call, one slot, one deadline for the
	// dial, the walk and the read together.
	if prof.Store == target.StoreSFTP {
		release, slotErr := f.client.Slot(ctx)
		if slotErr != nil {
			return nil, slotErr
		}
		defer release()
		if timeout := f.client.TimeoutFor(prof.Upstream()); timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
	}
	res, err := f.store(ctx, prof, read, p, budget)
	// A socket's own deadline may fire a moment before the context says it
	// is done: either is the store not answering in time.
	if err != nil && (errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)) {
		return nil, fmt.Errorf("%w: the %s store did not answer in time: %w", upstream.ErrTimeout, prof.Store, err)
	}
	return res, err
}

func (f *Fetcher) store(ctx context.Context, prof *target.Profile, read bool, p params, budget int) (*Result, error) {
	st, err := f.openStore(ctx, prof)
	if err != nil {
		return nil, err
	}
	defer st.close()
	root := prof.Root
	if root == "" {
		root = "/"
	}
	var last *storeEntry
	if st.follows() {
		if last, err = noLinks(ctx, st, root, p.Path); err != nil {
			return nil, err
		}
	}
	if !read {
		return f.listing(ctx, st, root, p, budget)
	}
	return f.readStore(ctx, st, root, prof.Redact, p, last, budget)
}

func (f *Fetcher) openStore(ctx context.Context, prof *target.Profile) (store, error) {
	switch prof.Store {
	case target.StoreWebDAV:
		return &davStore{client: f.client, t: prof.Upstream()}, nil
	case target.StoreSFTP:
		u, err := url.Parse(prof.BaseURL)
		if err != nil {
			return nil, err
		}
		addr := u.Host
		if u.Port() == "" {
			addr = net.JoinHostPort(u.Hostname(), "22")
		}
		conn, err := sftpfs.Dial(ctx, sftpfs.Config{
			Addr: addr, User: prof.SFTPUser, Password: prof.SFTPPassword, Key: []byte(prof.SFTPKey),
			HostKey: prof.SFTPHostKey, Timeout: sftpDialTimeout,
		})
		if err != nil {
			return nil, err
		}
		return &sftpStore{conn: conn}, nil
	}
	// The catalogue refuses an unknown kind at load time.
	return nil, fmt.Errorf("files: profile %q names no store this server knows", prof.Name())
}

// listing answers a directory with a line per entry: its size, when it was
// changed and its name — a directory with / after it, a symlink with @.
func (f *Fetcher) listing(ctx context.Context, st store, root string, p params, budget int) (*Result, error) {
	if p.Sheet != "" || p.Entry != "" {
		return nil, fmt.Errorf("%w: sheet and entry are for a file — /files/read", ErrBadParams)
	}
	entries, err := st.list(ctx, path.Join(root, p.Path))
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b storeEntry) int { return strings.Compare(a.name, b.name) })
	var buf bytes.Buffer
	for _, e := range entries {
		name := oneLine(e.name)
		switch {
		case e.dir:
			name += "/"
		case e.link:
			name += "@"
		}
		fmt.Fprintf(&buf, "%d\t%s\t%s\n", e.size, e.modified.UTC().Format("2006-01-02 15:04"), name)
	}
	meta := Meta{ID: p.Path, Name: p.Path, FileType: "directory"}
	out := &Text{Meta: meta, Header: contentsHeader}
	out.fill(buf.Bytes(), p, budget, false)
	return &Result{Meta: meta, Text: out}, nil
}

// readStore shows a file of the store: described first, refused before its
// bytes are read when it is not something this route shows. known is the
// file as the walk for links already described it, if it did.
func (f *Fetcher) readStore(ctx context.Context, st store, root, redactMode string, p params, known *storeEntry, budget int) (*Result, error) {
	var e storeEntry
	if known != nil {
		e = *known
	} else {
		var err error
		if e, err = st.stat(ctx, path.Join(root, p.Path)); err != nil {
			return nil, err
		}
	}
	switch {
	case e.dir:
		return nil, fmt.Errorf("%w: %s is a directory — list it with /files/list", ErrBadParams, p.Path)
	case e.link:
		return nil, refused(CodeUnsupported, "%s is a symlink, and links are not followed", p.Path)
	}
	name := path.Base(p.Path)
	meta := Meta{ID: p.Path, Name: name, FileType: extension(name), Size: e.size}
	if !e.modified.IsZero() {
		meta.Modified = e.modified.UTC().Format(time.RFC3339)
	}
	kind := classify(name, e.mimetype)
	if err := admit(meta, kind, e.mimetype, redactMode); err != nil {
		return nil, err
	}
	data, err := st.read(ctx, path.Join(root, p.Path))
	if err != nil {
		return nil, err
	}
	meta.Size = int64(len(data))
	return f.render(ctx, source{meta: meta, kind: kind, data: data}, p, budget, redactMode)
}

// noLinks walks the path down from Root and refuses it at the first symlink:
// resolving one would be asking the server where it points, and it may point
// out of Root. Root itself is the catalogue's word and is not checked. The
// last entry is returned for the caller not to ask about it again; nil for
// Root.
func noLinks(ctx context.Context, st store, root, rel string) (*storeEntry, error) {
	var last *storeEntry
	cur := root
	for seg := range strings.SplitSeq(strings.Trim(rel, "/"), "/") {
		if seg == "" {
			continue
		}
		cur = path.Join(cur, seg)
		e, err := st.stat(ctx, cur)
		if err != nil {
			return nil, err
		}
		if e.link {
			return nil, refused(CodeUnsupported, "%s goes through a symlink, and links are not followed", rel)
		}
		last = &e
	}
	return last, nil
}

// parseStoreParams reads the parameters of a store route: path instead of
// file. A listing without path is of Root; a file has to be named.
func parseStoreParams(rawPath string, read bool) (params, error) {
	p := params{Limit: defaultLimit}
	err := eachParam(rawPath, func(key, value string) error {
		switch key {
		case paramPath:
			clean, err := storePath(value)
			p.Path = clean
			return err
		case paramFile:
			return fmt.Errorf("%w: a store names files by path, not by id", ErrBadParams)
		}
		return p.set(key, value, fileID{})
	})
	switch {
	case err != nil:
		return p, err
	case p.Path == "" && read:
		return p, fmt.Errorf("%w: path is required — a file's path from /files/list", ErrBadParams)
	case p.Path == "":
		p.Path = "/"
	case p.Path == "/" && read:
		return p, fmt.Errorf("%w: / is a directory — list it with /files/list", ErrBadParams)
	}
	return p, nil
}

// storePath makes a path the model wrote into one below Root: absolute from
// the store's /, no . or .. step, no backslash, NUL or other control
// character. A trailing / is dropped.
func storePath(raw string) (string, error) {
	if raw == "" || utf8.RuneCountInString(raw) > maxStorePath {
		return "", fmt.Errorf("%w: path must be 1..%d characters", ErrBadParams, maxStorePath)
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", fmt.Errorf("%w: path %q has a backslash or a control character", ErrBadParams, raw)
		}
	}
	for seg := range strings.SplitSeq(raw, "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: path %q steps with %q — name the path from /", ErrBadParams, raw, seg)
		}
	}
	return path.Clean("/" + raw), nil
}

// notFound and the like are the store answering: the status says which.
func storeAnswered(status int, file string, err error) error {
	return answered(status, "%s: %s", file, err)
}

// --- WebDAV

// davStore reads a WebDAV collection through the profile's own upstream: the
// same client, credentials and limits as every call of it.
type davStore struct {
	client *upstream.Client
	t      upstream.Target
}

func (d *davStore) follows() bool { return false }
func (d *davStore) close()        {}

// davProps asks for what a listing shows.
const davProps = `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop>` +
	`<d:resourcetype/><d:getcontentlength/><d:getlastmodified/><d:getcontenttype/></d:prop></d:propfind>`

func (d *davStore) list(ctx context.Context, dir string) ([]storeEntry, error) {
	self := davURLPath(dir, true)
	entries, err := d.propfind(ctx, self, "1")
	if err != nil {
		return nil, err
	}
	out := make([]storeEntry, 0, len(entries))
	for _, e := range entries {
		if !e.self {
			out = append(out, e.entry)
		}
	}
	return out, nil
}

func (d *davStore) stat(ctx context.Context, file string) (storeEntry, error) {
	entries, err := d.propfind(ctx, davURLPath(file, false), "0")
	var up *UpstreamError
	if errors.As(err, &up) && up.Status >= 300 && up.Status < 400 {
		// A collection asked for without its slash is answered with a
		// redirect to it.
		return storeEntry{name: path.Base(file), dir: true}, nil
	}
	if err != nil {
		return storeEntry{}, err
	}
	for _, e := range entries {
		if e.self {
			return e.entry, nil
		}
	}
	return storeEntry{}, answered(http.StatusMultiStatus, "%s: the server described something else", file)
}

func (d *davStore) read(ctx context.Context, file string) ([]byte, error) {
	resp, err := d.client.Do(ctx, d.t, upstream.Request{
		Method: http.MethodGet, Path: davURLPath(file, false), MaxBytes: maxFileBytes,
	})
	switch {
	case err != nil:
		return nil, err
	case resp.Status != http.StatusOK:
		return nil, answered(resp.Status, "%s: the server answered %d", file, resp.Status)
	case resp.Truncated:
		return nil, refused(CodeTooLarge, "%s is over the %d bytes this server reads", file, maxFileBytes)
	}
	return resp.Body, nil
}

// davItem is one response of a PROPFIND, and whether it is about the path
// that was asked for.
type davItem struct {
	entry storeEntry
	self  bool
}

func (d *davStore) propfind(ctx context.Context, urlPath, depth string) ([]davItem, error) {
	resp, err := d.client.Do(ctx, d.t, upstream.Request{
		Method: "PROPFIND", Path: urlPath, Body: davProps, MaxBytes: davListMaxBytes,
		Headers: map[string]string{"Depth": depth, "Content-Type": "application/xml; charset=utf-8"},
	})
	switch {
	case err != nil:
		return nil, err
	case resp.Status != http.StatusMultiStatus:
		return nil, answered(resp.Status, "%s: the server answered %d", urlPath, resp.Status)
	case resp.Truncated:
		return nil, refused(CodeTooLarge, "the listing of %s is over %d bytes", urlPath, davListMaxBytes)
	}
	var ms davMultistatus
	if err := xml.Unmarshal(resp.Body, &ms); err != nil {
		return nil, answered(resp.Status, "%s: the listing does not parse: %s", urlPath, err)
	}
	want := strings.TrimSuffix(davDecoded(urlPath), "/")
	out := make([]davItem, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		p := strings.TrimSuffix(davDecoded(r.Href), "/")
		prop, ok := r.found()
		if !ok {
			continue
		}
		e := storeEntry{
			name: path.Base(p), dir: prop.ResourceType.Collection != nil, size: prop.Length,
			mimetype: prop.ContentType,
		}
		if t, err := http.ParseTime(prop.Modified); err == nil {
			e.modified = t
		}
		out = append(out, davItem{entry: e, self: p == want})
	}
	return out, nil
}

// davURLPath is a store path as the URL path to ask for: each segment
// escaped, a collection with its slash.
func davURLPath(full string, dir bool) string {
	segs := strings.Split(strings.Trim(full, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	out := "/" + strings.Join(segs, "/")
	if out == "/" {
		return out
	}
	if dir {
		out += "/"
	}
	return out
}

// davDecoded is the path of an href, which may be a whole URL, decoded.
func davDecoded(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	return u.Path
}

type davMultistatus struct {
	Responses []davResponse `xml:"DAV: response"`
}

type davResponse struct {
	Href      string `xml:"DAV: href"`
	Propstats []struct {
		Prop   davProp `xml:"DAV: prop"`
		Status string  `xml:"DAV: status"`
	} `xml:"DAV: propstat"`
}

type davProp struct {
	Length       int64  `xml:"DAV: getcontentlength"`
	Modified     string `xml:"DAV: getlastmodified"`
	ContentType  string `xml:"DAV: getcontenttype"`
	ResourceType struct {
		Collection *struct{} `xml:"DAV: collection"`
	} `xml:"DAV: resourcetype"`
}

// found is the properties the server had: those of the 200 propstat.
func (r davResponse) found() (davProp, bool) {
	for _, ps := range r.Propstats {
		if strings.Contains(ps.Status, " 200 ") || strings.HasSuffix(ps.Status, " 200") {
			return ps.Prop, true
		}
	}
	return davProp{}, false
}

// --- SFTP

type sftpStore struct {
	conn *sftpfs.Conn
}

func (s *sftpStore) follows() bool { return true }
func (s *sftpStore) close()        { _ = s.conn.Close() }

func (s *sftpStore) list(_ context.Context, dir string) ([]storeEntry, error) {
	infos, err := s.conn.ReadDir(dir)
	if err != nil {
		return nil, sftpError(dir, err)
	}
	out := make([]storeEntry, len(infos))
	for i, fi := range infos {
		out[i] = sftpEntry(fi)
	}
	return out, nil
}

func (s *sftpStore) stat(_ context.Context, file string) (storeEntry, error) {
	fi, err := s.conn.Lstat(file)
	if err != nil {
		return storeEntry{}, sftpError(file, err)
	}
	return sftpEntry(fi), nil
}

func (s *sftpStore) read(_ context.Context, file string) ([]byte, error) {
	data, err := s.conn.ReadFile(file, maxFileBytes)
	if errors.Is(err, sftpfs.ErrTooLarge) {
		return nil, refused(CodeTooLarge, "%s is over the %d bytes this server reads", file, maxFileBytes)
	}
	if err != nil {
		return nil, sftpError(file, err)
	}
	return data, nil
}

func sftpEntry(fi os.FileInfo) storeEntry {
	return storeEntry{
		name: fi.Name(), dir: fi.IsDir(), link: fi.Mode()&os.ModeSymlink != 0,
		size: fi.Size(), modified: fi.ModTime(),
	}
}

// sftpError gives a server's refusal the status an HTTP store would answer
// with: what is missing is 404, what may not be read is 403.
func sftpError(file string, err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, sftp.ErrSSHFxNoSuchFile):
		return storeAnswered(http.StatusNotFound, file, errors.New("no such file or directory"))
	case errors.Is(err, os.ErrPermission), errors.Is(err, sftp.ErrSSHFxPermissionDenied):
		return storeAnswered(http.StatusForbidden, file, errors.New("permission denied"))
	}
	return err
}
