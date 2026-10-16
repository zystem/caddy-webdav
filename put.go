package webdav

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// With `atomic_put` a PUT is written to a file in the `tmp_dir` directory of the root (the same file system), the file is flushed (`sync`) and renamed to the name of the object,
// and the directory is flushed. A crash or a client that goes away leaves a file in `tmp_dir` (removed when it is older than `tmp_age`) and never a part of an object under its name.
// A path that leads into `tmp_dir` is not found, for every method.

const (
	syncFull = "full" // the file and then its directory are flushed before the answer: a write that was answered survives a power cut
	syncFile = "file" // the file only
	syncNone = "none" // nothing: the page cache decides

	copyBuffer = 1 << 20
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
	existed := false
	if st, err := os.Lstat(full); err == nil {
		if st.IsDir() {
			http.Error(w, "the name is a collection", http.StatusConflict)
			return
		}
		existed = true
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
	n, err := io.CopyBuffer(f, r.Body, *bp)
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
	if err := os.Rename(tmp, full); err != nil {
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
	w.Header().Set("Content-Length", "0")
	if existed {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}
