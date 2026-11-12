package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	pluginA = "app.example.a"
	pluginB = "app.example.b"
)

type fixture struct {
	t    *testing.T
	root string
	srv  *server
}

func newFixture(t *testing.T) *fixture {
	root := t.TempDir()
	for _, id := range []string{pluginA, pluginB} {
		if err := os.Mkdir(filepath.Join(root, id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &fixture{t: t, root: root, srv: &server{root: root}}
}

// do sends a request as plugin id ("" sends no X-Wonton-Plugin). Headers come in name, value pairs.
func (f *fixture) do(method, target, id string, body io.Reader, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	if id != "" {
		req.Header.Set("X-Wonton-Plugin", id)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) put(target, body string, headers ...string) *httptest.ResponseRecorder {
	return f.do(http.MethodPut, target, pluginA, strings.NewReader(body), headers...)
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status %d, want %d: %s", rec.Code, code, rec.Body.String())
	}
}

func TestPutGetRoundTripChangesETag(t *testing.T) {
	f := newFixture(t)
	created := f.put("/todo.json", `{"a":1}`)
	expect(t, created, http.StatusCreated)
	get := f.do(http.MethodGet, "/todo.json", pluginA, nil)
	expect(t, get, http.StatusOK)
	if get.Body.String() != `{"a":1}` || get.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("body %q type %q", get.Body, get.Header().Get("Content-Type"))
	}
	if get.Header().Get("ETag") != created.Header().Get("ETag") {
		t.Fatalf("GET ETag %s differs from PUT ETag %s", get.Header().Get("ETag"), created.Header().Get("ETag"))
	}
	overwritten := f.put("/todo.json", `{"a":2}`)
	expect(t, overwritten, http.StatusNoContent)
	if overwritten.Header().Get("ETag") == created.Header().Get("ETag") {
		t.Fatal("ETag did not change on overwrite")
	}
	head := f.do(http.MethodHead, "/todo.json", pluginA, nil)
	expect(t, head, http.StatusOK)
	if head.Body.Len() != 0 || head.Header().Get("ETag") != overwritten.Header().Get("ETag") {
		t.Fatalf("HEAD body %q ETag %s", head.Body, head.Header().Get("ETag"))
	}
	expect(t, f.do(http.MethodGet, "/todo.json", pluginA, nil, "If-None-Match", overwritten.Header().Get("ETag")),
		http.StatusNotModified)
	unknown := f.put("/blob.bin", "<html>")
	expect(t, unknown, http.StatusCreated)
	if ct := f.do(http.MethodGet, "/blob.bin", pluginA, nil).Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("unknown extension served as %q", ct)
	}
}

func TestConditionalWrites(t *testing.T) {
	f := newFixture(t)
	expect(t, f.put("/new.json", "1", "If-Match", `"nope"`), http.StatusPreconditionFailed)
	tag := f.put("/x.json", "1", "If-None-Match", "*")
	expect(t, tag, http.StatusCreated)
	expect(t, f.put("/x.json", "2", "If-None-Match", "*"), http.StatusPreconditionFailed)
	expect(t, f.put("/x.json", "2", "If-Match", `"stale"`), http.StatusPreconditionFailed)
	expect(t, f.put("/x.json", "2", "If-Match", "W/"+tag.Header().Get("ETag")), http.StatusPreconditionFailed)
	if got := f.do(http.MethodGet, "/x.json", pluginA, nil).Body.String(); got != "1" {
		t.Fatalf("failed preconditions changed the file to %q", got)
	}
	expect(t, f.put("/x.json", "2", "If-Match", tag.Header().Get("ETag")), http.StatusNoContent)
	expect(t, f.do(http.MethodDelete, "/x.json", pluginA, nil, "If-Match", tag.Header().Get("ETag")),
		http.StatusPreconditionFailed)
}

func TestPutRules(t *testing.T) {
	f := newFixture(t)
	expect(t, f.put("/missing/x.json", "1"), http.StatusConflict)
	expect(t, f.put("/x.json", "1", "Content-Range", "bytes 0-0/1"), http.StatusNotImplemented)
	expect(t, f.put("/", "1"), http.StatusMethodNotAllowed)
	expect(t, f.do("MKCOL", "/dir", pluginA, nil), http.StatusCreated)
	expect(t, f.put("/dir", "1"), http.StatusMethodNotAllowed)
	expect(t, f.put("/dir/x.json", "1"), http.StatusCreated)
}

func TestQuotaLeavesOriginalIntact(t *testing.T) {
	f := newFixture(t)
	expect(t, f.put("/keep.json", "original"), http.StatusCreated)
	big := bytes.Repeat([]byte("x"), maxFile+1)
	expect(t, f.do(http.MethodPut, "/keep.json", pluginA, bytes.NewReader(big)), http.StatusInsufficientStorage)

	// No Content-Length: the limit has to hold while streaming.
	req := httptest.NewRequest(http.MethodPut, "/keep.json", io.NopCloser(bytes.NewReader(big)))
	req.ContentLength = -1
	req.Header.Set("X-Wonton-Plugin", pluginA)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	expect(t, rec, http.StatusInsufficientStorage)

	if got := f.do(http.MethodGet, "/keep.json", pluginA, nil).Body.String(); got != "original" {
		t.Fatalf("original became %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(f.root, pluginA))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}

	// Total: eight 8 MiB files fill 64 MiB, so one more byte is over.
	full := bytes.Repeat([]byte("y"), maxFile)
	for i := range 7 {
		expect(t, f.do(http.MethodPut, "/f"+strconv.Itoa(i), pluginA, bytes.NewReader(full)), http.StatusCreated)
	}
	expect(t, f.do(http.MethodPut, "/last", pluginA, bytes.NewReader(full[:maxFile-len("original")])), http.StatusCreated)
	expect(t, f.put("/one-more", "z"), http.StatusInsufficientStorage)
	// Shrinking an existing file still works when the total is full.
	expect(t, f.put("/keep.json", "short"), http.StatusNoContent)
}

func TestEntryLimit(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, pluginA)
	for i := range maxEntries {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expect(t, f.put("/new", "1"), http.StatusInsufficientStorage)
	expect(t, f.do("MKCOL", "/newdir", pluginA, nil), http.StatusInsufficientStorage)
	expect(t, f.put("/f0", "overwrite is fine"), http.StatusNoContent)
}

func TestDeleteAndMkcol(t *testing.T) {
	f := newFixture(t)
	expect(t, f.do("MKCOL", "/d", pluginA, nil), http.StatusCreated)
	expect(t, f.do("MKCOL", "/d", pluginA, nil), http.StatusMethodNotAllowed)
	expect(t, f.do("MKCOL", "/nope/d", pluginA, nil), http.StatusConflict)
	expect(t, f.do("MKCOL", "/e", pluginA, strings.NewReader("<x/>")), http.StatusUnsupportedMediaType)
	expect(t, f.put("/d/x.json", "1"), http.StatusCreated)
	expect(t, f.do(http.MethodDelete, "/d", pluginA, nil), http.StatusConflict)
	expect(t, f.do(http.MethodDelete, "/d/x.json", pluginA, nil), http.StatusNoContent)
	expect(t, f.do(http.MethodGet, "/d/x.json", pluginA, nil), http.StatusNotFound)
	expect(t, f.do(http.MethodDelete, "/d/x.json", pluginA, nil), http.StatusNotFound)
	expect(t, f.do(http.MethodDelete, "/d", pluginA, nil), http.StatusNoContent)
	expect(t, f.do(http.MethodDelete, "/", pluginA, nil), http.StatusMethodNotAllowed)
}

func TestPropfind(t *testing.T) {
	f := newFixture(t)
	expect(t, f.do("MKCOL", "/notes", pluginA, nil), http.StatusCreated)
	expect(t, f.put("/notes/a%20b.json", "12345"), http.StatusCreated)
	expect(t, f.do("PROPFIND", "/notes", pluginA, nil), http.StatusNotImplemented)
	expect(t, f.do("PROPFIND", "/notes", pluginA, nil, "Depth", "infinity"), http.StatusNotImplemented)
	expect(t, f.do("PROPFIND", "/nope", pluginA, nil, "Depth", "0"), http.StatusNotFound)

	one := f.do("PROPFIND", "/notes", pluginA, strings.NewReader("<ignored/>"), "Depth", "1")
	expect(t, one, http.StatusMultiStatus)
	body := one.Body.String()
	for _, want := range []string{
		"<D:href>notes/</D:href>", "<D:collection></D:collection>",
		"<D:href>notes/a%20b.json</D:href>", "<D:getcontentlength>5</D:getcontentlength>",
		"<D:getetag>&#34;", "<D:getlastmodified>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Depth 1 response lacks %s:\n%s", want, body)
		}
	}
	zero := f.do("PROPFIND", "/notes/", pluginA, nil, "Depth", "0")
	expect(t, zero, http.StatusMultiStatus)
	if strings.Count(zero.Body.String(), "<D:response>") != 1 || !strings.Contains(zero.Body.String(), "<D:href>./</D:href>") {
		t.Fatalf("Depth 0:\n%s", zero.Body)
	}
}

func TestMethodsAndHeader(t *testing.T) {
	f := newFixture(t)
	for _, m := range []string{"LOCK", "UNLOCK", "MOVE", "COPY", "PROPPATCH"} {
		expect(t, f.do(m, "/x", pluginA, nil), http.StatusMethodNotAllowed)
	}
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodOptions, "SEARCH"} {
		expect(t, f.do(m, "/x", pluginA, nil), http.StatusNotImplemented)
	}
	expect(t, f.do(http.MethodGet, "/x", "", nil), http.StatusBadRequest)
	for _, bad := range []string{"..", "noDot", "App.example", "a..b", ".a.b", "a.b/c", "a.b,c.d", "a." + strings.Repeat("b", 62)} {
		expect(t, f.do(http.MethodGet, "/x", bad, nil), http.StatusBadRequest)
	}
	two := httptest.NewRequest(http.MethodGet, "/x", nil)
	two.Header["X-Wonton-Plugin"] = []string{pluginA, pluginB}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, two)
	expect(t, rec, http.StatusBadRequest)
	expect(t, f.do(http.MethodGet, "/x", "app.example.missing", nil), http.StatusNotFound)
	if _, err := os.Stat(filepath.Join(f.root, "app.example.missing")); err == nil {
		t.Fatal("the server created a plugin root")
	}
	expect(t, f.do(http.MethodGet, "/healthz", "", nil), http.StatusOK)
}

func TestIsolationAndTraversal(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, pluginB, "secret.json"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, pluginA, ".hidden"), []byte("h"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.do(http.MethodGet, "/secret.json", pluginA, nil), http.StatusNotFound)
	for _, target := range []string{
		"/../app.example.b/secret.json", "/%2e%2e/app.example.b/secret.json", "/..%2fapp.example.b%2fsecret.json",
		"/x/%2e%2e/%2e%2e/app.example.b/secret.json", "/.hidden", "/a//b", "/%2fetc%2fpasswd",
	} {
		rec := f.do(http.MethodGet, target, pluginA, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s: %d %q", target, rec.Code, rec.Body)
		}
	}
	expect(t, f.do(http.MethodPut, "/..%2fapp.example.b%2fsecret.json", pluginA, strings.NewReader("pwned")), http.StatusBadRequest)
	if b, _ := os.ReadFile(filepath.Join(f.root, pluginB, "secret.json")); string(b) != "b" {
		t.Fatalf("B's file became %q", b)
	}
	listing := f.do("PROPFIND", "/", pluginA, nil, "Depth", "1").Body.String()
	if strings.Contains(listing, "hidden") {
		t.Fatalf("PROPFIND lists dotfiles:\n%s", listing)
	}
}

func TestLeftoverTempFilesAreRemoved(t *testing.T) {
	f := newFixture(t)
	tmp := filepath.Join(f.root, pluginA, tmpPrefix+"123")
	if err := os.WriteFile(tmp, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeLeftovers(f.root)
	if _, err := os.Stat(tmp); err == nil {
		t.Fatal("leftover temp file still there")
	}
}
