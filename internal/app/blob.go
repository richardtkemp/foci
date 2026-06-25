package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/internal/app/fap"
	"foci/internal/log"
	"foci/internal/tempdir"
)

// Blob store tuning (wire-protocol §9). Defaults; config wiring (max_blob_mb /
// blob_ttl) lands with the config slice.
const (
	maxBlobBytes        = 50 << 20 // upload cap
	blobTTL             = 24 * time.Hour
	blobReapInterval    = time.Hour
	inlineAttachmentMax = 16 << 20 // inbound attachments below this are loaded into Attachment.Data
)

var errBlobTooLarge = errors.New("app: blob exceeds size limit")

// blobMeta describes one stored blob.
type blobMeta struct {
	id      string
	path    string
	mime    string
	kind    string
	name    string
	size    int64
	created time.Time
}

// blobStore is the out-of-band media store backing /app/blob (§9). Blobs live on
// disk under a dedicated dir; metadata is held in memory. They are short-TTL and
// size-capped, and never travel over the WebSocket — the control stream carries
// only `media {blobId,…}` / `message.attachments` references.
type blobStore struct {
	dir      string
	maxBytes int64
	ttl      time.Duration

	mu    sync.Mutex
	blobs map[string]*blobMeta
}

func newBlobStore() *blobStore {
	dir := filepath.Join(tempdir.Dir(), "app-blobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Warnf("app", "blob store dir %s: %v", dir, err)
	}
	return &blobStore{dir: dir, maxBytes: maxBlobBytes, ttl: blobTTL, blobs: make(map[string]*blobMeta)}
}

// put streams data from r into a new blob file, enforcing the size cap. kind /
// name / mime are recorded for the outbound media frame or inbound attachment.
func (s *blobStore) put(r io.Reader, kind, name, mimeType string) (*blobMeta, error) {
	id := fap.NewULID()
	path := filepath.Join(s.dir, id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	// LimitReader to maxBytes+1 so we can detect an over-cap payload.
	n, copyErr := io.Copy(f, io.LimitReader(r, s.maxBytes+1))
	closeErr := f.Close()
	switch {
	case copyErr != nil:
		_ = os.Remove(path)
		return nil, copyErr
	case closeErr != nil:
		_ = os.Remove(path)
		return nil, closeErr
	case n > s.maxBytes:
		_ = os.Remove(path)
		return nil, errBlobTooLarge
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	meta := &blobMeta{id: id, path: path, mime: mimeType, kind: kind, name: name, size: n, created: time.Now()}
	s.mu.Lock()
	s.blobs[id] = meta
	s.mu.Unlock()
	return meta, nil
}

// putFile copies an on-disk file (a foci-produced media path) into the store,
// decoupling the blob's lifetime from the caller's temp file.
func (s *blobStore) putFile(srcPath, kind string) (*blobMeta, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	name := filepath.Base(srcPath)
	return s.put(f, kind, name, mimeByName(name))
}

// putBytes stores an in-memory payload (e.g. TTS audio).
func (s *blobStore) putBytes(data []byte, kind, name, mimeType string) (*blobMeta, error) {
	return s.put(bytes.NewReader(data), kind, name, mimeType)
}

func (s *blobStore) get(id string) (*blobMeta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.blobs[id]
	return m, ok
}

// reap removes blobs older than the TTL (both files and metadata).
func (s *blobStore) reap() {
	cutoff := time.Now().Add(-s.ttl)
	s.mu.Lock()
	var stale []*blobMeta
	for id, m := range s.blobs {
		if m.created.Before(cutoff) {
			stale = append(stale, m)
			delete(s.blobs, id)
		}
	}
	s.mu.Unlock()
	for _, m := range stale {
		_ = os.Remove(m.path)
	}
}

// reaper periodically evicts expired blobs until ctx is cancelled.
func (s *blobStore) reaper(ctx context.Context) {
	t := time.NewTicker(blobReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reap()
		}
	}
}

// mimeByName guesses a MIME type from a filename extension.
func mimeByName(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// kindForMIME maps a MIME type to a FAP media kind (§9) — used for uploads,
// where the client sends a MIME but no explicit kind.
func kindForMIME(m string) string {
	switch {
	case strings.HasPrefix(m, "image/gif"):
		return fap.MediaAnimation
	case strings.HasPrefix(m, "image/"):
		return fap.MediaPhoto
	case strings.HasPrefix(m, "audio/"):
		return fap.MediaAudio
	case strings.HasPrefix(m, "video/"):
		return fap.MediaVideo
	default:
		return fap.MediaDocument
	}
}

// --- HTTP handlers (Bearer app.api_key, same gate as /app/ws) ---

// ServeBlobGet handles GET /app/blob/<id>: authenticates, then serves the blob
// range-capably (http.ServeContent) so the client can resume partial fetches.
func (h *Hub) ServeBlobGet(w http.ResponseWriter, r *http.Request) {
	if !h.authBlob(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/app/blob/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "bad blob id", http.StatusBadRequest)
		return
	}
	meta, ok := h.blobs.get(id)
	if !ok {
		http.Error(w, "blob not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(meta.path)
	if err != nil {
		http.Error(w, "blob unavailable", http.StatusGone)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", meta.mime)
	http.ServeContent(w, r, meta.name, meta.created, f)
}

// ServeBlobPost handles POST /app/blob: authenticates, stores the request body
// as a new blob (cap-enforced), and returns {blobId,size,mime}.
func (h *Hub) ServeBlobPost(w http.ResponseWriter, r *http.Request) {
	if !h.authBlob(w, r) {
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mimeType := r.Header.Get("Content-Type")
	name := r.Header.Get("X-Filename")
	meta, err := h.blobs.put(r.Body, kindForMIME(mimeType), name, mimeType)
	if err != nil {
		if errors.Is(err, errBlobTooLarge) {
			http.Error(w, "blob too large", http.StatusRequestEntityTooLarge)
			return
		}
		log.Errorf("app", "blob upload: %v", err)
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"blobId": meta.id, "size": meta.size, "mime": meta.mime})
}

// authBlob enforces the master key OR a valid device token on a blob request
// (rate-limited), writing the error response and returning false on failure.
func (h *Hub) authBlob(w http.ResponseWriter, r *http.Request) bool {
	_, ok := h.authenticate(w, r)
	return ok
}
