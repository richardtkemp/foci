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
	"foci/internal/tempdir"
)

// Blob store tuning (wire-protocol §9). These are the code defaults;
// [platforms.app] max_blob_mb / blob_ttl override them via the config cascade.
const (
	defaultMaxBlobMB    = 50                     // upload cap, MB
	maxBlobBytes        = defaultMaxBlobMB << 20 // upload cap, bytes
	defaultBlobTTL      = 24 * time.Hour         // blob time-to-live
	blobTTL             = defaultBlobTTL         // default store TTL
	blobReapInterval    = time.Hour              // reaper tick
	inlineAttachmentMax = 16 << 20               // inbound attachments below this are loaded into Attachment.Data
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
		appLog.Warnf("blob store dir %s: %v", dir, err)
	}
	s := &blobStore{dir: dir, maxBytes: maxBlobBytes, ttl: blobTTL, blobs: make(map[string]*blobMeta)}
	s.rehydrate()
	return s
}

// rehydrate scans s.dir at construction time and rebuilds a blobMeta for
// every file already there, so blobs written before a restart stay servable
// and stay visible to reap() — instead of becoming permanent, unreapable
// dead weight (the whole reason this method exists: #1500).
//
// What's recoverable vs. not, honestly:
//   - created: decoded straight from the id, which is a ULID (48-bit ms
//     timestamp + entropy, fap.NewULID) — exact, no fallback needed. A
//     filename that doesn't decode as a well-formed ULID was never written
//     by put()/putFile()/putBytes(), so it isn't one of ours; it's left
//     alone (not registered, not deleted) rather than guessed at via stat,
//     which would make an unrelated file dropped in the dir web-servable.
//   - size: from stat — exact.
//   - mime: NOT recoverable from the id or filename (there's no sidecar).
//     Sniffed from the first 512 bytes via http.DetectContentType — a fixed,
//     bounded read per file so this stays cheap even for a large blob (the
//     size cap is 50MB; some real files here are ~18MB APKs) and for a large
//     directory (3000+ files is normal in production).
//   - kind: derived from the sniffed mime via the existing kindForMIME.
//   - name: genuinely lost — the original upload filename was never
//     persisted anywhere on disk, only held in the in-memory meta that a
//     restart just erased. Using the id itself as name rather than inventing
//     one; this only affects the http.ServeContent name/ext hint, since
//     ServeBlobGet already sets the Content-Type header explicitly from
//     meta.mime before calling it.
//
// A file already past TTL at startup is reaped immediately rather than
// resurrected. maxBytes (the upload-time size cap) is deliberately NOT
// re-enforced here: it gates new uploads, not retention of files already on
// disk — a config change that lowers the cap shouldn't punitively delete
// pre-existing blobs at the next restart. They still age out via the normal
// TTL reaper like everything else.
func (s *blobStore) rehydrate() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		appLog.Warnf("blob store rehydrate: reading %s: %v", s.dir, err)
		return
	}
	cutoff := time.Now().Add(-s.ttl)
	var kept, reaped, skipped int
	for _, e := range entries {
		if e.IsDir() {
			skipped++
			continue
		}
		id := e.Name()
		created, ok := fap.ULIDTime(id)
		if !ok {
			// Not a ULID -> not something put() wrote. Leave it untouched.
			skipped++
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Raced with a concurrent delete, or unreadable -- skip, don't crash.
			skipped++
			continue
		}
		path := filepath.Join(s.dir, id)
		if created.Before(cutoff) {
			_ = os.Remove(path)
			reaped++
			continue
		}
		mimeType := sniffMime(path)
		s.blobs[id] = &blobMeta{
			id:      id,
			path:    path,
			mime:    mimeType,
			kind:    kindForMIME(mimeType),
			name:    id,
			size:    info.Size(),
			created: created,
		}
		kept++
	}
	if kept+reaped+skipped > 0 {
		appLog.Infof("blob store rehydrate: %d recovered, %d expired, %d skipped (%s)", kept, reaped, skipped, s.dir)
	}
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

// sniffMime reads a bounded prefix of path and detects its MIME type, for
// rehydrating a blob whose original upload MIME wasn't persisted anywhere on
// disk. Falls back to DetectContentType's own generic default on read error.
func sniffMime(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return http.DetectContentType(buf[:n])
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

// --- HTTP handlers (Bearer device token, same gate as /app/ws) ---

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
		// The request is authenticated with a valid device token and the id is a
		// well-formed blob ref the client saw in history — so an absent blob means it
		// existed and was reaped by TTL, not a missing resource. Return 410 Gone (not
		// 404): it's the correct semantics AND keeps clients off scanner-detection
		// scenarios (CrowdSec http-probing et al. count 404/403/400 bursts, not 410)
		// when a synced client re-fetches expired media across a whole conversation.
		http.Error(w, "blob expired", http.StatusGone)
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
		appLog.Errorf("blob upload: %v", err)
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"blobId": meta.id, "size": meta.size, "mime": meta.mime})
}

// authBlob enforces a valid device token on a blob request
// (rate-limited), writing the error response and returning false on failure.
func (h *Hub) authBlob(w http.ResponseWriter, r *http.Request) bool {
	_, ok := h.authenticate(w, r)
	return ok
}
