package spill

import (
	"os"
	"sync"
	"testing"
)

func TestUnderThreshold(t *testing.T) {
	// Verifies that writes below the threshold stay entirely in the head
	// buffer with no temp file created.
	t.Parallel()
	sw := New(100, 0, t.TempDir())
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

func TestAtThreshold(t *testing.T) {
	// Verifies that writes up to exactly the threshold remain in memory.
	t.Parallel()
	data := make([]byte, 50)
	for i := range data {
		data[i] = 'x'
	}
	sw := New(50, 0, t.TempDir())
	defer sw.Cleanup()

	sw.Write(data)
	if sw.Spilled() {
		t.Error("should not have spilled at exactly threshold")
	}
	if sw.Total() != 50 {
		t.Errorf("Total = %d, want 50", sw.Total())
	}
}

func TestOverflow(t *testing.T) {
	// Verifies that exceeding the threshold creates a temp file with the full
	// output, and String() returns the head portion.
	t.Parallel()
	threshold := int64(10)
	sw := New(threshold, 0, t.TempDir())
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

	head := sw.String()
	if head != "1234567890" {
		t.Errorf("String = %q, want %q", head, "1234567890")
	}

	data, err := os.ReadFile(sw.FilePath())
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if string(data) != "12345678901234567890" {
		t.Errorf("spill file = %q, want full data", string(data))
	}
}

func TestMultipleWrites(t *testing.T) {
	// Verifies multiple small writes transition from head buffer to temp file
	// when cumulative size exceeds the threshold.
	t.Parallel()
	sw := New(8, 0, t.TempDir())
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

func TestConcurrent(t *testing.T) {
	// Verifies thread safety with concurrent writes.
	t.Parallel()
	sw := New(100, 0, t.TempDir())
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

func TestCleanup(t *testing.T) {
	// Verifies that Cleanup removes the temp file.
	t.Parallel()
	sw := New(5, 0, t.TempDir())
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

func TestMaxTotalCap(t *testing.T) {
	// Verifies that maxTotal caps retained bytes: the overflow is dropped, the
	// on-disk file holds only up to the cap, Total reflects the full source
	// length, and Truncated reports true. (The HTTP DoS-ceiling case.)
	t.Parallel()
	sw := New(4, 10, t.TempDir()) // preview 4 bytes, hard cap 10 bytes
	defer sw.Cleanup()

	// Write 16 bytes from a "remote" source; only 10 should be retained.
	n, err := sw.Write([]byte("0123456789ABCDEF"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 16 {
		t.Errorf("Write returned %d, want 16 (full source length acknowledged)", n)
	}
	if !sw.Truncated() {
		t.Error("should report Truncated after exceeding maxTotal")
	}
	if sw.Total() != 16 {
		t.Errorf("Total = %d, want 16 (full source length)", sw.Total())
	}
	if !sw.Spilled() {
		t.Fatal("should have spilled (10 > preview 4)")
	}
	data, err := os.ReadFile(sw.FilePath())
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if string(data) != "0123456789" {
		t.Errorf("spill file = %q, want first 10 bytes only", string(data))
	}
}

func TestMaxTotalNotHit(t *testing.T) {
	// Verifies that when the source stays under maxTotal, nothing is truncated.
	t.Parallel()
	sw := New(4, 100, t.TempDir())
	defer sw.Cleanup()
	sw.Write([]byte("short"))
	if sw.Truncated() {
		t.Error("should not be truncated under the cap")
	}
	if sw.Total() != 5 {
		t.Errorf("Total = %d, want 5", sw.Total())
	}
}
