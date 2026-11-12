// wonton-web-storage: the web_storage capability. A WebDAV (RFC 4918) whole-file subset with RFC 9110
// conditionals over <root>/<X-Wonton-Plugin>/, with per-plugin quotas answered by 507.
//
// The core gateway authenticates, deletes every client X-Wonton-* header and writes X-Wonton-Plugin, so this
// server does no auth and picks the plugin directory from that header only, never from the path or Host.
// The core creates <root>/<plugin-id>/ at install time; this server never creates it.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxTotal   = 64 << 20 // bytes of regular files per plugin
	maxEntries = 1024     // files and directories per plugin: directories count too, or MKCOL is unbounded
	maxFile    = 8 << 20
	// Temp files sit next to their target so the rename is atomic. Clients can never name them: every
	// path segment starting with "." is rejected.
	tmpPrefix  = ".wonton-put-"
	notAllowed = "LOCK, UNLOCK, MOVE, COPY, PROPPATCH"
)

var pluginID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*(\.[a-z0-9]+(-[a-z0-9]+)*)+$`)

type server struct {
	root  string
	locks sync.Map // plugin root -> *sync.Mutex: writes in one plugin run one at a time, so quotas hold
}

func main() {
	root := flag.String("root", "/srv/data", "directory holding one subdirectory per plugin")
	addr := flag.String("listen", ":8081", "listen address")
	check := flag.Bool("healthcheck", false, "request /healthz on -listen and exit 0 when healthy")
	flag.Parse()
	if *check {
		os.Exit(healthcheck(*addr))
	}
	if fi, err := os.Stat(*root); err != nil || !fi.IsDir() {
		log.Fatalf("data root %s is not a directory (is the core's web_storage mount missing?): %v", *root, err)
	}
	removeLeftovers(*root)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           &server{root: *root},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	log.Fatal(srv.ListenAndServe())
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ids, has := r.Header["X-Wonton-Plugin"]
	if !has {
		// The gateway always sets the header, so /healthz is only reachable from inside the container.
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			s.health(w)
			return
		}
		fail(w, http.StatusBadRequest, "missing X-Wonton-Plugin")
		return
	}
	if len(ids) != 1 || len(ids[0]) > 63 || !pluginID.MatchString(ids[0]) {
		fail(w, http.StatusBadRequest, "invalid X-Wonton-Plugin")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, "MKCOL", "PROPFIND":
	case "LOCK", "UNLOCK", "MOVE", "COPY", "PROPPATCH":
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE, MKCOL, PROPFIND")
		fail(w, http.StatusMethodNotAllowed, r.Method+" is not supported")
		return
	default:
		fail(w, http.StatusNotImplemented, r.Method+" is not supported")
		return
	}
	segs, slash, ok := segments(r)
	if !ok {
		fail(w, http.StatusBadRequest, "path segments must be non-empty and must not start with . or contain an encoded /")
		return
	}
	root := filepath.Join(s.root, ids[0])
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		fail(w, http.StatusNotFound, "no storage for this plugin")
		return
	}
	p := filepath.Join(append([]string{root}, segs...)...)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		get(w, r, p)
	case http.MethodPut:
		s.put(w, r, root, p, len(segs) == 0 || slash)
	case http.MethodDelete:
		s.remove(w, r, root, p, len(segs) == 0)
	case "MKCOL":
		s.mkcol(w, r, root, p, len(segs) == 0)
	case "PROPFIND":
		propfind(w, r, p, segs, slash)
	}
}

// segments decodes the request path one segment at a time, so an encoded "/" stays inside its segment and
// is rejected instead of becoming a separator. "..", "." and dotfiles are rejected the same way.
func segments(r *http.Request) (segs []string, slash, ok bool) {
	raw, found := strings.CutPrefix(r.URL.EscapedPath(), "/")
	if !found {
		return nil, false, false
	}
	if raw == "" {
		return nil, true, true
	}
	raw, slash = strings.CutSuffix(raw, "/")
	for _, part := range strings.Split(raw, "/") {
		seg, err := url.PathUnescape(part)
		if err != nil || seg == "" || seg[0] == '.' || strings.ContainsAny(seg, "/\x00") {
			return nil, false, false
		}
		segs = append(segs, seg)
	}
	return segs, slash, true
}

func get(w http.ResponseWriter, r *http.Request, p string) {
	f, err := os.Open(p)
	if err != nil {
		failFS(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		failFS(w, err)
		return
	}
	if fi.IsDir() {
		w.Header().Set("Allow", "PROPFIND, MKCOL, DELETE")
		fail(w, http.StatusMethodNotAllowed, "a collection has no body: use PROPFIND")
		return
	}
	tag, err := etag(f)
	if err != nil {
		failFS(w, err)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		failFS(w, err)
		return
	}
	w.Header().Set("ETag", tag)
	w.Header().Set("Content-Type", contentType(p))
	// ServeContent answers If-Match, If-None-Match, If-Modified-Since, Range and HEAD.
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func (s *server) put(w http.ResponseWriter, r *http.Request, root, p string, collection bool) {
	if r.Header.Get("Content-Range") != "" {
		fail(w, http.StatusNotImplemented, "partial PUT (Content-Range) is not supported")
		return
	}
	if collection {
		fail(w, http.StatusMethodNotAllowed, "cannot PUT a collection")
		return
	}
	defer s.lock(root)()
	if !parentIsDir(p) {
		fail(w, http.StatusConflict, "parent collection does not exist")
		return
	}
	old, err := os.Lstat(p)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		failFS(w, err)
		return
	}
	if exists && !old.Mode().IsRegular() {
		fail(w, http.StatusMethodNotAllowed, "cannot PUT over a collection")
		return
	}
	if !checkPreconditions(w, r, p, exists) {
		return
	}
	used, entries, err := usage(root)
	if err != nil {
		failFS(w, err)
		return
	}
	var oldSize int64
	if exists {
		oldSize = old.Size()
	}
	room := min(maxFile, maxTotal-(used-oldSize))
	if (!exists && entries >= maxEntries) || r.ContentLength > room {
		fail(w, http.StatusInsufficientStorage, quotaText)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), tmpPrefix+"*")
	if err != nil {
		failFS(w, err)
		return
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	h := sha256.New()
	// Enforced while streaming too: chunked bodies have no Content-Length.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r.Body, room+1))
	var pathErr *fs.PathError
	switch {
	case err != nil && errors.As(err, &pathErr):
		tmp.Close()
		failFS(w, err)
		return
	case err != nil:
		tmp.Close()
		fail(w, http.StatusBadRequest, "reading the request body failed: "+err.Error())
		return
	case n > room:
		tmp.Close()
		fail(w, http.StatusInsufficientStorage, quotaText)
		return
	}
	if err := errors.Join(tmp.Chmod(0o644), tmp.Sync(), tmp.Close()); err != nil {
		failFS(w, err)
		return
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		failFS(w, err)
		return
	}
	w.Header().Set("ETag", quote(h.Sum(nil)))
	if exists {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *server) remove(w http.ResponseWriter, r *http.Request, root, p string, isRoot bool) {
	if isRoot {
		fail(w, http.StatusMethodNotAllowed, "cannot delete the plugin's storage root")
		return
	}
	defer s.lock(root)()
	fi, err := os.Lstat(p)
	if err != nil {
		failFS(w, err)
		return
	}
	if fi.Mode().IsRegular() && !checkPreconditions(w, r, p, true) {
		return
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			fail(w, http.StatusConflict, "collection is not empty")
			return
		}
		failFS(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) mkcol(w http.ResponseWriter, r *http.Request, root, p string, isRoot bool) {
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		fail(w, http.StatusUnsupportedMediaType, "MKCOL with a body is not supported")
		return
	}
	if isRoot {
		fail(w, http.StatusMethodNotAllowed, "already exists")
		return
	}
	defer s.lock(root)()
	if _, err := os.Lstat(p); err == nil {
		fail(w, http.StatusMethodNotAllowed, "already exists")
		return
	}
	if !parentIsDir(p) {
		fail(w, http.StatusConflict, "parent collection does not exist")
		return
	}
	_, entries, err := usage(root)
	if err != nil {
		failFS(w, err)
		return
	}
	if entries >= maxEntries {
		fail(w, http.StatusInsufficientStorage, quotaText)
		return
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		failFS(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// checkPreconditions evaluates If-Match and If-None-Match (RFC 9110 §13) for a write; it writes 412 and
// returns false when they fail. Hashing only happens when the request is conditional.
func checkPreconditions(w http.ResponseWriter, r *http.Request, p string, exists bool) bool {
	ifMatch := strings.Join(r.Header.Values("If-Match"), ",")
	ifNoneMatch := strings.Join(r.Header.Values("If-None-Match"), ",")
	if ifMatch == "" && ifNoneMatch == "" {
		return true
	}
	current := ""
	if exists {
		f, err := os.Open(p)
		if err != nil {
			failFS(w, err)
			return false
		}
		current, err = etag(f)
		f.Close()
		if err != nil {
			failFS(w, err)
			return false
		}
	}
	ok := (ifMatch == "" || current != "" && tagListHas(ifMatch, current)) &&
		(ifNoneMatch == "" || current == "" || !tagListHas(ifNoneMatch, current))
	if !ok {
		fail(w, http.StatusPreconditionFailed, "precondition failed")
	}
	return ok
}

// tagListHas compares strongly: our tags are strong, so a W/ tag never matches.
func tagListHas(list, tag string) bool {
	for _, t := range strings.Split(list, ",") {
		if t = strings.TrimSpace(t); t == "*" || t == tag {
			return true
		}
	}
	return false
}

// etag is a content hash. Tags built from inode, size and mtime can repeat when two writes of the same size
// land in one coarse timestamp tick and reuse a freed inode, which would let a stale If-Match through.
func etag(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return quote(h.Sum(nil)), nil
}

func quote(sum []byte) string { return `"` + hex.EncodeToString(sum[:16]) + `"` }

func contentType(p string) string {
	if t := mime.TypeByExtension(filepath.Ext(p)); t != "" {
		return t
	}
	// Never sniff: data written by a page must not come back as text/html.
	return "application/octet-stream"
}

func parentIsDir(p string) bool {
	fi, err := os.Stat(filepath.Dir(p))
	return err == nil && fi.IsDir()
}

// usage counts what the quota covers: bytes of regular files and the number of entries below root.
func usage(root string) (bytes int64, entries int, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		entries++
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			bytes += fi.Size()
		}
		return nil
	})
	return bytes, entries, err
}

// removeLeftovers deletes temp files a crash left behind; they count against the quota and no client
// can name them to delete them.
func removeLeftovers(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && strings.HasPrefix(d.Name(), tmpPrefix) {
			if err := os.Remove(p); err != nil {
				log.Printf("removing leftover %s: %v", p, err)
			}
		}
		return nil
	})
}

func (s *server) lock(root string) (unlock func()) {
	m, _ := s.locks.LoadOrStore(root, new(sync.Mutex))
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *server) health(w http.ResponseWriter) {
	fi, err := os.Stat(s.root)
	if err == nil && !fi.IsDir() {
		err = fmt.Errorf("not a directory")
	}
	if err == nil {
		err = syscall.Access(s.root, 0x2) // W_OK
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "data root "+s.root+" is not a writable directory: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func healthcheck(addr string) int {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, resp.Status)
		return 1
	}
	return 0
}

const quotaText = "over the per-plugin quota: 64 MiB, 1024 files and directories, 8 MiB per file"

func fail(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}

// failFS maps a file system error to a status; anything unexpected is logged and answered with 500.
func failFS(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		fail(w, http.StatusNotFound, "not found")
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		fail(w, http.StatusInsufficientStorage, "the disk is full")
	case errors.Is(err, syscall.ENAMETOOLONG):
		fail(w, http.StatusBadRequest, "name too long")
	default:
		log.Print(err)
		fail(w, http.StatusInternalServerError, "storage error")
	}
}
