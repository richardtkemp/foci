package tools

import (
	"os"
	"sync"
	"testing"
)

func TestSpillWriterUnderThreshold(t *testing.T) {
	// TestSpillWriterUnderThreshold verifies that writes below the threshold
	// stay entirely in the head buffer with no temp file created.
	t.Parallel()
	sw := newSpillWriter(100, t.TempDir())
	defer sw.Cleanup()

	n, err := sw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 5 {
		t.Errorf("Write returned %d, want 5", n)
	}
	if sw.Spilled() {
		t.Error("should not have spilled")
	}
	if sw.FilePath() != "" {
		t.Error("should have no file path")
	}
	if sw.Total() != 5 {
		t.Errorf("Total = %d, want 5", sw.Total())
	}
	if sw.String() != "hello" {
		t.Errorf("String = %q, want %q", sw.String(), "hello")
	}
}

func TestSpillWriterAtThreshold(t *testing.T) {
	// TestSpillWriterAtThreshold verifies that writes up to exactly the threshold
	// remain in memory without spilling.
	t.Parallel()
	data := make([]byte, 50)
	for i := range data {
		data[i] = 'x'
	}
	sw := newSpillWriter(50, t.TempDir())
	defer sw.Cleanup()

	sw.Write(data)
	if sw.Spilled() {
		t.Error("should not have spilled at exactly threshold")
	}
	if sw.Total() != 50 {
		t.Errorf("Total = %d, want 50", sw.Total())
	}
}

func TestSpillWriterOverflow(t *testing.T) {
	// TestSpillWriterOverflow verifies that exceeding the threshold creates a temp
	// file containing the full output, and String() returns the head portion.
	t.Parallel()
	threshold := int64(10)
	sw := newSpillWriter(threshold, t.TempDir())
	defer sw.Cleanup()

	sw.Write([]byte("12345678901234567890")) // 20 bytes, exceeds threshold of 10
	if !sw.Spilled() {
		t.Fatal("should have spilled")
	}
	if sw.FilePath() == "" {
		t.Fatal("should have a file path")
	}
	if sw.Total() != 20 {
		t.Errorf("Total = %d, want 20", sw.Total())
	}

	// String returns head portion (first threshold bytes)
	head := sw.String()
	if head != "1234567890" {
		t.Errorf("String = %q, want %q", head, "1234567890")
	}

	// Full output is in the file
	data, err := os.ReadFile(sw.FilePath())
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if string(data) != "12345678901234567890" {
		t.Errorf("spill file = %q, want full data", string(data))
	}
}

func TestSpillWriterMultipleWrites(t *testing.T) {
	// TestSpillWriterMultipleWrites verifies that multiple small writes correctly
	// transition from head buffer to temp file when cumulative size exceeds threshold.
	t.Parallel()
	sw := newSpillWriter(8, t.TempDir())
	defer sw.Cleanup()

	sw.Write([]byte("abc"))  // 3, under
	sw.Write([]byte("def"))  // 6, under
	sw.Write([]byte("ghij")) // 10, over

	if !sw.Spilled() {
		t.Fatal("should have spilled")
	}
	if sw.Total() != 10 {
		t.Errorf("Total = %d, want 10", sw.Total())
	}
	if sw.String() != "abcdefgh" {
		t.Errorf("String = %q, want %q", sw.String(), "abcdefgh")
	}

	data, _ := os.ReadFile(sw.FilePath())
	if string(data) != "abcdefghij" {
		t.Errorf("spill file = %q, want %q", string(data), "abcdefghij")
	}
}

func TestSpillWriterConcurrent(t *testing.T) {
	// TestSpillWriterConcurrent verifies thread safety with concurrent writes.
	t.Parallel()
	sw := newSpillWriter(100, t.TempDir())
	defer sw.Cleanup()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sw.Write([]byte("xx")) // 2 bytes each = 100 total
		}()
	}
	wg.Wait()

	if sw.Total() != 100 {
		t.Errorf("Total = %d, want 100", sw.Total())
	}
}

func TestSpillWriterCleanup(t *testing.T) {
	// TestSpillWriterCleanup verifies that Cleanup removes the temp file.
	t.Parallel()
	sw := newSpillWriter(5, t.TempDir())
	sw.Write([]byte("1234567890")) // spill
	if !sw.Spilled() {
		t.Fatal("should have spilled")
	}
	path := sw.FilePath()
	sw.Cleanup()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("temp file should be removed after Cleanup, err=%v", err)
	}
}
