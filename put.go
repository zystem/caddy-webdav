package webdav

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// With `atomic_put` a PUT is written to a file in the `tmp_dir` directory of the root (the same file system), the file is flushed (`sync`) and renamed to the name of the object,
// and the directory is flushed. A crash or a client that goes away leaves a file in `tmp_dir` (removed when it is older than `tmp_age`) and never a part of an object under its name.
// A path that leads into `tmp_dir` is not found, for every method.
//
// `If-None-Match: *` makes the PUT create-only (412 when the object is there); the check and the creation are one operation (a hard link), not two.
//
// A client that sends the SHA-256 of what it sends in `X-Content-SHA256` has it checked while the body is written: on a mismatch (422) nothing is placed. The answer carries
// the SHA-256 of what was written in the same header.
//
// `GET` and `HEAD` with the query `sha256` answer the SHA-256 of an object in the header `X-Content-SHA256` (and in the body of a GET), from an extended attribute that holds it
// together with the size and the time of the file it was taken from, and is taken again when they differ.

const (
	syncFull = "full" // the file and then its directory are flushed before the answer: a write that was answered survives a power cut
	syncFile = "file" // the file only
	syncNone = "none" // nothing: the page cache decides

	copyBuffer = 1 << 20
	hashHeader = "X-Content-SHA256"
	hashAttr   = "user.cinim.sha256"
)

var bufPool = sync.Pool{New: func() any { b := make([]byte, copyBuffer); return &b }}

// insideTmp: the request is for the directory of unfinished writes, which nobody sees
func (wd *WebDAV) insideTmp(rel string) bool {
	first := strings.SplitN(strings.TrimPrefix(rel, "/"), "/", 2)[0]
	return first == wd.tmpDir()
}

func (wd *WebDAV) tmpDir() string {
	if wd.TmpDir == "" {
		return ".tmp"
	}
	return wd.TmpDir
}

func (wd *WebDAV) syncMode() string {
	if wd.Sync == "" {
		return syncFull
	}
	return wd.Sync
}

func (wd *WebDAV) tmpAge() time.Duration {
	if wd.TmpAge == 0 {
		return 24 * time.Hour
	}
	return time.Duration(wd.TmpAge)
}

func fsyncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

func validHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// sweepTmp removes the files in `dir` older than `tmp_age`, at most once an hour for a directory. The directories that have been swept are remembered for that hour in a table of
// bounded size, so that a root that follows the request (a placeholder) cannot make it grow, or start a scan for every new name, without limit: past the bound a directory is simply
// not swept until the table has room again.
const maxSweptDirs = 4096

type sweepLog struct {
	mu sync.Mutex
	at map[string]time.Time
}

var sweeps = sweepLog{at: map[string]time.Time{}}

// due says whether `dir` is to be swept now, and notes that it is
func (l *sweepLog) due(dir string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.at[dir]; ok {
		if now.Sub(t) < time.Hour {
			return false
		}
	} else if len(l.at) >= maxSweptDirs {
		for d, t := range l.at {
			if now.Sub(t) >= time.Hour {
				delete(l.at, d)
			}
		}
		if len(l.at) >= maxSweptDirs {
			return false
		}
	}
	l.at[dir] = now
	return true
}

func (l *sweepLog) forget(dir string) {
	l.mu.Lock()
	delete(l.at, dir)
	l.mu.Unlock()
}

func (wd *WebDAV) sweepTmp(dir string) {
	now := time.Now()
	if !sweeps.due(dir, now) {
		return
	}
	go func() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if info, err := e.Info(); err == nil && !info.IsDir() && now.Sub(info.ModTime()) > wd.tmpAge() {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}()
}

func (wd *WebDAV) put(w http.ResponseWriter, r *http.Request, root, prefix string) {
	rel, full, ok := resolve(root, prefix, r.URL.Path)
	if !ok {
		http.Error(w, "not under the prefix of the share", http.StatusNotFound)
		return
	}
	if rel == "/" || strings.HasSuffix(r.URL.Path, "/") {
		http.Error(w, "a PUT is for an object, not for a collection", http.StatusMethodNotAllowed)
		return
	}
	if wd.insideTmp(rel) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Header.Get("Content-Range") != "" {
		http.Error(w, "a PUT with Content-Range is not supported", http.StatusNotImplemented)
		return
	}
	announced := strings.ToLower(strings.TrimSpace(r.Header.Get(hashHeader)))
	if announced != "" && !validHex(announced) {
		http.Error(w, hashHeader+" is 64 hexadecimal digits", http.StatusBadRequest)
		return
	}
	createOnly := strings.TrimSpace(r.Header.Get("If-None-Match")) == "*"
	existed := false
	if st, err := os.Lstat(full); err == nil {
		if st.IsDir() {
			http.Error(w, "the name is a collection", http.StatusConflict)
			return
		}
		existed = true
		if createOnly {
			http.Error(w, "the object exists", http.StatusPreconditionFailed)
			return
		}
	}

	tmpDir := filepath.Join(filepath.Clean(root), wd.tmpDir())
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		wd.logger.Error("cannot make the directory of unfinished writes", zap.Error(err))
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	wd.sweepTmp(tmpDir)
	var rnd [12]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(tmpDir, "put-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		wd.logger.Error("cannot make a file for a write", zap.Error(err))
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	done := false
	defer func() {
		if !done {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	bp := bufPool.Get().(*[]byte)
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(f, h), r.Body, *bp)
	bufPool.Put(bp)
	if err != nil {
		switch {
		case errors.Is(err, syscall.ENOSPC):
			http.Error(w, "the disk is full", http.StatusInsufficientStorage)
		default:
			// the client went away or sent less than it said; nothing was placed
			http.Error(w, "the body could not be read: "+err.Error(), http.StatusBadRequest)
		}
		return
	}
	if r.ContentLength >= 0 && n != r.ContentLength {
		http.Error(w, "the body is shorter than Content-Length", http.StatusBadRequest)
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if announced != "" && announced != sum {
		http.Error(w, hashHeader+" does not match the body (got "+sum+")", http.StatusUnprocessableEntity)
		return
	}
	if wd.syncMode() != syncNone {
		if err := f.Sync(); err != nil {
			wd.logger.Error("cannot flush a file", zap.Error(err))
			http.Error(w, "storage error", http.StatusInsufficientStorage)
			return
		}
	}
	if err := f.Close(); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	dir := filepath.Dir(full)
	if wd.CreateDirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EEXIST) {
				http.Error(w, "a part of the name is an object", http.StatusConflict)
			} else {
				wd.logger.Error("cannot make the parent directories", zap.Error(err))
				http.Error(w, "storage error", http.StatusInternalServerError)
			}
			return
		}
	} else if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		http.Error(w, "the parent collection does not exist", http.StatusConflict)
		return
	}
	if createOnly {
		// the check and the creation in one step: a link fails if the name is taken
		if err := os.Link(tmp, full); err != nil {
			if errors.Is(err, fs.ErrExist) {
				http.Error(w, "the object exists", http.StatusPreconditionFailed)
			} else {
				wd.logger.Error("cannot place an object", zap.Error(err))
				http.Error(w, "storage error", http.StatusInternalServerError)
			}
			return
		}
		_ = os.Remove(tmp)
	} else if err := os.Rename(tmp, full); err != nil {
		if errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENOTEMPTY) {
			http.Error(w, "the name is a collection", http.StatusConflict)
		} else {
			wd.logger.Error("cannot place an object", zap.Error(err))
			http.Error(w, "storage error", http.StatusInternalServerError)
		}
		return
	}
	done = true
	if wd.syncMode() == syncFull {
		fsyncDir(dir)
	}
	if st, err := os.Stat(full); err == nil {
		storeHash(full, sum, st)
	}
	w.Header().Set(hashHeader, sum)
	w.Header().Set("Content-Length", "0")
	if existed {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

// hashOf answers the SHA-256 of an object (GET or HEAD with the query `sha256`)
func (wd *WebDAV) hashOf(w http.ResponseWriter, r *http.Request, root, prefix string) {
	rel, full, ok := resolve(root, prefix, r.URL.Path)
	if !ok || wd.insideTmp(rel) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	st, err := os.Stat(full)
	if err != nil || !st.Mode().IsRegular() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sum, ok := cachedHash(full, st)
	if !ok {
		f, err := os.Open(full)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h := sha256.New()
		bp := bufPool.Get().(*[]byte)
		_, err = io.CopyBuffer(h, f, *bp)
		bufPool.Put(bp)
		_ = f.Close()
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		sum = hex.EncodeToString(h.Sum(nil))
		if st2, err := os.Stat(full); err == nil && st2.Size() == st.Size() && st2.ModTime().Equal(st.ModTime()) {
			storeHash(full, sum, st)
		}
	}
	w.Header().Set(hashHeader, sum)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(sum)+1))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, sum+"\n")
	}
}

func attrValue(sum string, st os.FileInfo) string {
	return sum + ":" + strconv.FormatInt(st.Size(), 10) + ":" + strconv.FormatInt(st.ModTime().UnixNano(), 10)
}

func storeHash(file, sum string, st os.FileInfo) { setAttr(file, hashAttr, attrValue(sum, st)) }

func cachedHash(file string, st os.FileInfo) (string, bool) {
	v, ok := getAttr(file, hashAttr)
	if !ok {
		return "", false
	}
	i := strings.IndexByte(v, ':')
	if i != 64 || attrValue(v[:i], st) != v {
		return "", false
	}
	return v[:i], true
}
