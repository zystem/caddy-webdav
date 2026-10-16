package webdav

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// newHandler makes a module with its root in a directory below a scratch one (so that a write outside the root can be seen), and a function that serves a request.
func newHandler(t *testing.T, tune ...func(*WebDAV)) (root, outside string, do func(method, target string, body io.Reader, hdr ...string) *httptest.ResponseRecorder) {
	t.Helper()
	outside = t.TempDir()
	root = filepath.Join(outside, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := &WebDAV{Root: root}
	for _, f := range tune {
		f(wd)
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := wd.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error { w.WriteHeader(http.StatusTeapot); return nil })
	do = func(method, target string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://store"+target, body)
		r.URL.Path = target
		if i := strings.IndexByte(target, '?'); i >= 0 {
			r.URL.Path, r.URL.RawQuery = target[:i], target[i+1:]
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
			if hdr[i] == "Content-Length" {
				r.ContentLength, _ = strconv.ParseInt(hdr[i+1], 10, 64)
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
		w := httptest.NewRecorder()
		if err := wd.ServeHTTP(w, r, next); err != nil {
			t.Fatal(err)
		}
		return w
	}
	return
}

func createDirs(w *WebDAV) { w.CreateDirs = true }

func TestCreateDirsMakesTheParents(t *testing.T) {
	root, _, do := newHandler(t, createDirs)
	if w := do("PUT", "/acme/r1/dist/app.txt", strings.NewReader("hello")); w.Code != http.StatusCreated {
		t.Fatalf("code %d", w.Code)
	}
	if got, err := os.ReadFile(filepath.Join(root, "acme/r1/dist/app.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("content: %v %q", err, got)
	}
}

func TestWithoutCreateDirsAMissingParentIsRefused(t *testing.T) {
	root, _, do := newHandler(t)
	if w := do("PUT", "/acme/r1/app.txt", strings.NewReader("hello")); w.Code < 400 {
		t.Fatalf("code %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "acme")); err == nil {
		t.Fatal("a directory was made")
	}
}

func TestCreateDirsNeverLeavesTheRoot(t *testing.T) {
	_, outside, do := newHandler(t, createDirs)
	for _, p := range []string{"/../../escape1/x.bin", "/a/../../../escape2/x.bin", "/./../escape3/x.bin", "//../escape4/x.bin"} {
		do("PUT", p, strings.NewReader("x"))
	}
	entries, _ := os.ReadDir(outside)
	for _, e := range entries {
		if e.Name() != "root" {
			t.Fatalf("%q was made outside the root", e.Name())
		}
	}
}

func TestCreateDirsKeepsToThePrefix(t *testing.T) {
	root, _, do := newHandler(t, createDirs, func(w *WebDAV) { w.Prefix = "/store" })
	if w := do("PUT", "/store/a/b", strings.NewReader("x")); w.Code != http.StatusCreated {
		t.Fatalf("under the prefix: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	do("PUT", "/storeX/c/d", strings.NewReader("x"))
	do("PUT", "/other/e/f", strings.NewReader("x"))
	for _, d := range []string{"c", "e", "storeX", "other"} {
		if _, err := os.Stat(filepath.Join(root, d)); err == nil {
			t.Fatalf("%q was made for a path outside the prefix", d)
		}
	}
}

func atomic(w *WebDAV) { w.AtomicPut = true }

func tmpEntries(t *testing.T, root string) int {
	t.Helper()
	e, err := os.ReadDir(filepath.Join(root, ".tmp"))
	if err != nil {
		return 0
	}
	return len(e)
}

func TestAtomicPutMakesTheObject(t *testing.T) {
	root, _, do := newHandler(t, atomic, createDirs)
	data := []byte("hello object")
	w := do("PUT", "/acme/r1/dist/app.txt", bytes.NewReader(data))
	if w.Code != http.StatusCreated {
		t.Fatalf("created: %d %v", w.Code, w.Header())
	}
	got, err := os.ReadFile(filepath.Join(root, "acme/r1/dist/app.txt"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("content: %v %q", err, got)
	}
	if n := tmpEntries(t, root); n != 0 {
		t.Fatalf("%d leftovers in tmp", n)
	}
	// a second PUT replaces it
	w = do("PUT", "/acme/r1/dist/app.txt", strings.NewReader("second"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("replace: %d", w.Code)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "acme/r1/dist/app.txt")); string(got) != "second" {
		t.Fatalf("replaced: %q", got)
	}
}

func TestAtomicPutWithoutCreateDirsNeedsTheParent(t *testing.T) {
	root, _, do := newHandler(t, atomic)
	if w := do("PUT", "/a/b", strings.NewReader("x")); w.Code != http.StatusConflict {
		t.Fatalf("missing parent: %d", w.Code)
	}
	if n := tmpEntries(t, root); n != 0 {
		t.Fatalf("%d leftovers in tmp", n)
	}
	_ = os.Mkdir(filepath.Join(root, "a"), 0o755)
	if w := do("PUT", "/a/b", strings.NewReader("x")); w.Code != http.StatusCreated {
		t.Fatalf("with the parent: %d", w.Code)
	}
}

type brokenBody struct{ sent bool }

func (b *brokenBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "half of an obj"), nil
	}
	return 0, errors.New("connection reset")
}

func TestAWriteThatBreaksLeavesNothingUnderTheName(t *testing.T) {
	root, _, do := newHandler(t, atomic, createDirs)
	if w := do("PUT", "/k/big", &brokenBody{}); w.Code != http.StatusBadRequest {
		t.Fatalf("code %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "k/big")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a part of an object is there: %v", err)
	}
	if n := tmpEntries(t, root); n != 0 {
		t.Fatalf("%d leftovers in tmp", n)
	}
	// a body shorter than Content-Length
	if w := do("PUT", "/k/short", io.LimitReader(strings.NewReader("12345"), 5), "Content-Length", "10"); w.Code != http.StatusBadRequest {
		t.Fatalf("short body: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "k/short")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a short body was placed: %v", err)
	}
	// an old object stays whole when the write that was to replace it breaks
	do("PUT", "/k/old", strings.NewReader("old content"))
	do("PUT", "/k/old", &brokenBody{})
	if got, _ := os.ReadFile(filepath.Join(root, "k/old")); string(got) != "old content" {
		t.Fatalf("old object damaged: %q", got)
	}
}

func TestTwoWritersOfOneKeyNeverMix(t *testing.T) {
	root, _, do := newHandler(t, atomic, createDirs)
	a, b := make([]byte, 4<<20), make([]byte, 4<<20)
	_, _ = rand.Read(a)
	_, _ = rand.Read(b)
	for round := 0; round < 10; round++ {
		var wg sync.WaitGroup
		for _, p := range [][]byte{a, b} {
			wg.Add(1)
			go func() { defer wg.Done(); do("PUT", "/race.bin", bytes.NewReader(p)) }()
		}
		wg.Wait()
		got, err := os.ReadFile(filepath.Join(root, "race.bin"))
		if err != nil || (!bytes.Equal(got, a) && !bytes.Equal(got, b)) {
			t.Fatalf("round %d: the object is neither of the two (%v)", round, err)
		}
	}
}

func TestAtomicPutNeverLeavesTheRoot(t *testing.T) {
	_, outside, do := newHandler(t, atomic, createDirs)
	for _, p := range []string{"/../../escape1/x.bin", "/a/../../../escape2/x.bin", "/./../escape3/x.bin", "//../escape4/x.bin"} {
		do("PUT", p, strings.NewReader("x"))
	}
	entries, _ := os.ReadDir(outside)
	for _, e := range entries {
		if e.Name() != "root" {
			t.Fatalf("%q was made outside the root", e.Name())
		}
	}
}

func TestTheDirectoryOfUnfinishedWritesIsNotReachable(t *testing.T) {
	root, _, do := newHandler(t, atomic, createDirs)
	if w := do("PUT", "/.tmp/x", strings.NewReader("x")); w.Code != http.StatusNotFound {
		t.Fatalf("put into tmp: %d", w.Code)
	}
	if w := do("PUT", "/a/../.tmp/x", strings.NewReader("x")); w.Code != http.StatusNotFound {
		t.Fatalf("put into tmp by a detour: %d", w.Code)
	}
	if w := do("PROPFIND", "/.tmp/", nil, "Depth", "1"); w.Code != http.StatusNotFound {
		t.Fatalf("propfind tmp: %d", w.Code)
	}
	// nor as the destination of a MOVE or a COPY
	do("PUT", "/m", strings.NewReader("an object"))
	for _, method := range []string{"MOVE", "COPY"} {
		for _, dst := range []string{"http://store/.tmp/y", "/.tmp/y", "http://store/a/../.tmp/y"} {
			if w := do(method, "/m", nil, "Destination", dst); w.Code != http.StatusNotFound {
				t.Fatalf("%s into tmp (%s): %d", method, dst, w.Code)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "m")); err != nil {
		t.Fatal("the source of the refused MOVE is gone")
	}
	if n := tmpEntries(t, root); n != 0 {
		t.Fatalf("%d entries", n)
	}
}

func TestAtomicPutConflictsAreRefused(t *testing.T) {
	_, _, do := newHandler(t, atomic, createDirs)
	do("PUT", "/a/b", strings.NewReader("an object"))
	if w := do("PUT", "/a/b/c", strings.NewReader("x")); w.Code != http.StatusConflict {
		t.Fatalf("below an object: %d", w.Code)
	}
	if w := do("PUT", "/a", strings.NewReader("x")); w.Code != http.StatusConflict {
		t.Fatalf("over a collection: %d", w.Code)
	}
	if w := do("PUT", "/a/", strings.NewReader("x")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("a collection name: %d", w.Code)
	}
}

func TestLeftoversOfOldWritesAreSwept(t *testing.T) {
	root, _, do := newHandler(t, atomic, createDirs)
	tmp := filepath.Join(root, ".tmp")
	_ = os.MkdirAll(tmp, 0o755)
	old, young := filepath.Join(tmp, "put-old"), filepath.Join(tmp, "put-young")
	for _, f := range []string{old, young} {
		_ = os.WriteFile(f, []byte("x"), 0o644)
	}
	past := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(old, past, past)
	sweeps.forget(tmp)
	do("PUT", "/x", strings.NewReader("x"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(old); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old leftover is still there")
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatal("a young file was removed")
	}
}

func TestTheTableOfSweptDirectoriesIsBounded(t *testing.T) {
	var l = sweepLog{at: map[string]time.Time{}}
	now := time.Now()
	for i := 0; i < maxSweptDirs; i++ {
		if !l.due("/d/"+strconv.Itoa(i), now) {
			t.Fatalf("directory %d was not due", i)
		}
	}
	if l.due("/d/0", now.Add(time.Minute)) {
		t.Fatal("a directory swept a minute ago is due")
	}
	if l.due("/d/new", now.Add(time.Minute)) || len(l.at) != maxSweptDirs {
		t.Fatalf("the table grew past its bound: %d", len(l.at))
	}
	// an hour later the entries are no use and make room
	if !l.due("/d/new", now.Add(time.Hour+time.Second)) {
		t.Fatal("a new directory was not swept when the table had room")
	}
	if len(l.at) > maxSweptDirs {
		t.Fatalf("the table is over its bound: %d", len(l.at))
	}
	if !l.due("/d/3", now.Add(2*time.Hour)) {
		t.Fatal("a directory is not swept again an hour later")
	}
}

func TestAtomicPutKeepsToThePrefix(t *testing.T) {
	root, _, do := newHandler(t, atomic, createDirs, func(w *WebDAV) { w.Prefix = "/store" })
	if w := do("PUT", "/store/a/b", strings.NewReader("x")); w.Code != http.StatusCreated {
		t.Fatalf("under the prefix: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "a/b")); err != nil {
		t.Fatal(err)
	}
	if w := do("PUT", "/storeX/a", strings.NewReader("x")); w.Code != http.StatusNotFound {
		t.Fatalf("not the prefix: %d", w.Code)
	}
	if w := do("PUT", "/other/a", strings.NewReader("x")); w.Code != http.StatusNotFound {
		t.Fatalf("outside the prefix: %d", w.Code)
	}
}

func TestSettings(t *testing.T) {
	d := caddyfile.NewTestDispenser(`webdav {
		root /data
		prefix /s
		create_dirs
		atomic_put
		sync file
		tmp_dir .partial
		tmp_age 2h
	}`)
	wd := new(WebDAV)
	if err := wd.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if wd.Root != "/data" || wd.Prefix != "/s" || !wd.CreateDirs {
		t.Fatalf("%+v", wd)
	}
	if !wd.AtomicPut || wd.Sync != "file" || wd.TmpDir != ".partial" || time.Duration(wd.TmpAge) != 2*time.Hour {
		t.Fatalf("%+v", wd)
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	for _, bad := range []*WebDAV{{Sync: "sometimes"}, {TmpDir: "a/b"}, {TmpDir: ".."}} {
		if err := bad.Provision(ctx); err == nil {
			t.Fatalf("%+v was accepted", bad)
		}
	}
}

func TestCreateOnlyPutHasOneWinner(t *testing.T) {
	root, _, do := newHandler(t, atomic, createDirs)
	if w := do("PUT", "/once", strings.NewReader("first"), "If-None-Match", "*"); w.Code != http.StatusCreated {
		t.Fatalf("first: %d", w.Code)
	}
	if w := do("PUT", "/once", strings.NewReader("second"), "If-None-Match", "*"); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("second: %d", w.Code)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "once")); string(got) != "first" {
		t.Fatalf("content %q", got)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := do("PUT", "/race", strings.NewReader(strings.Repeat("x", 1000)), "If-None-Match", "*")
			mu.Lock()
			codes[w.Code]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[http.StatusCreated] != 1 || codes[http.StatusPreconditionFailed] != 19 {
		t.Fatalf("racers: %v", codes)
	}
	if n := tmpEntries(t, root); n != 0 {
		t.Fatalf("%d leftovers in tmp", n)
	}
}

func shaOf(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestPutChecksTheAnnouncedHash(t *testing.T) {
	root, _, do := newHandler(t, atomic, createDirs)
	data := []byte("hello object")
	w := do("PUT", "/a/ok", bytes.NewReader(data), hashHeader, shaOf(data))
	if w.Code != http.StatusCreated || w.Header().Get(hashHeader) != shaOf(data) {
		t.Fatalf("created: %d %v", w.Code, w.Header())
	}
	w = do("PUT", "/a/b", strings.NewReader("data"), hashHeader, shaOf([]byte("other")))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "a/b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an object was placed: %v", err)
	}
	if n := tmpEntries(t, root); n != 0 {
		t.Fatalf("%d leftovers in tmp", n)
	}
	if w := do("PUT", "/a/b", strings.NewReader("data"), hashHeader, "nothex"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad header: %d", w.Code)
	}
	// the announced hash may be in capitals, and a PUT without one is answered with the hash of what was written
	if w := do("PUT", "/a/b", strings.NewReader("data"), hashHeader, strings.ToUpper(shaOf([]byte("data")))); w.Code != http.StatusCreated {
		t.Fatalf("capitals: %d", w.Code)
	}
	if w := do("PUT", "/a/c", strings.NewReader("data")); w.Header().Get(hashHeader) != shaOf([]byte("data")) {
		t.Fatalf("no hash in the answer: %v", w.Header())
	}
}
