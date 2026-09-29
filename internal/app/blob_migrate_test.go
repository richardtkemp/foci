package app

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"foci/internal/config"
	"foci/internal/fap"
	"foci/internal/platform"
)

// #1556: the blob store lives under the durable data dir, not the temp root
// (/tmp/foci), which a host reboot wipes.
func TestNewHub_BlobStoreUnderDataDir(t *testing.T) {
	dataDir := t.TempDir()
	h := newHub(platform.ProviderDeps{Config: &config.Config{DataDir: dataDir}})

	want := filepath.Join(dataDir, "app-blobs")
	if h.blobs.dir != want {
		t.Fatalf("blob dir = %q, want %q", h.blobs.dir, want)
	}
	meta, err := h.blobs.putBytes([]byte("hi"), "document", "f.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(meta.path) != want {
		t.Errorf("blob path = %q, want it under %q", meta.path, want)
	}
}

func writeBlobFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMigrateBlobDir_MovesBlobsAndTheyRehydrate(t *testing.T) {
	from := filepath.Join(t.TempDir(), "app-blobs")
	to := filepath.Join(t.TempDir(), "app-blobs") // not yet created: migration makes it

	id := fap.NewULID()
	src := writeBlobFile(t, from, id, "blob bytes")
	junk := writeBlobFile(t, from, "not-a-ulid.txt", "x")
	linkID := fap.NewULID()
	if err := os.Symlink(junk, filepath.Join(from, linkID)); err != nil {
		t.Fatal(err)
	}

	moved, failed := migrateBlobDir(from, to)
	if moved != 1 || failed != 0 {
		t.Fatalf("moved=%d failed=%d, want 1/0", moved, failed)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source blob still present after migration: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(to, id)); err != nil || string(got) != "blob bytes" {
		t.Errorf("migrated blob = %q (%v), want %q", got, err, "blob bytes")
	}
	// Non-blob entries are not ours to move: they stay for CleanStale.
	if _, err := os.Stat(junk); err != nil {
		t.Errorf("non-ULID file was moved: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(to, linkID)); !os.IsNotExist(err) {
		t.Errorf("symlink with a ULID name was migrated: %v", err)
	}

	// TTL/rehydrate behaviour carries over: a store over the new dir serves it.
	s := newBlobStore(to)
	if m, ok := s.get(id); !ok || m.size != int64(len("blob bytes")) {
		t.Errorf("migrated blob not rehydrated: ok=%v m=%+v", ok, m)
	}
}

func TestMigrateBlobDir_NeverOverwrites(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	id := fap.NewULID()
	src := writeBlobFile(t, from, id, "legacy")
	writeBlobFile(t, to, id, "already here")

	moved, failed := migrateBlobDir(from, to)
	if moved != 0 || failed != 1 {
		t.Fatalf("moved=%d failed=%d, want 0/1", moved, failed)
	}
	if got, _ := os.ReadFile(filepath.Join(to, id)); string(got) != "already here" {
		t.Errorf("existing destination blob overwritten: %q", got)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source removed despite failed move: %v", err)
	}
}

func TestMigrateBlobDir_CrossFilesystemCopies(t *testing.T) {
	orig := blobLink
	blobLink = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EXDEV} }
	t.Cleanup(func() { blobLink = orig })

	from, to := t.TempDir(), t.TempDir()
	id := fap.NewULID()
	src := writeBlobFile(t, from, id, "copied")

	moved, failed := migrateBlobDir(from, to)
	if moved != 1 || failed != 0 {
		t.Fatalf("moved=%d failed=%d, want 1/0", moved, failed)
	}
	if got, err := os.ReadFile(filepath.Join(to, id)); err != nil || string(got) != "copied" {
		t.Errorf("copied blob = %q (%v)", got, err)
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("source kept after cross-filesystem move: %v", err)
	}
}

func TestMigrateBlobDir_MissingLegacyDirIsNoop(t *testing.T) {
	to := filepath.Join(t.TempDir(), "app-blobs")
	if moved, failed := migrateBlobDir(filepath.Join(t.TempDir(), "absent"), to); moved+failed != 0 {
		t.Errorf("moved=%d failed=%d, want 0/0", moved, failed)
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Errorf("destination created for a missing legacy dir: %v", err)
	}
}
