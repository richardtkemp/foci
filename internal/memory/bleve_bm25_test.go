package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	index "github.com/blevesearch/bleve_index_api"
)

// scoringModelOf returns the scoring model stored in an open index's mapping.
func scoringModelOf(t *testing.T, b *BleveIndex) string {
	t.Helper()
	im, ok := b.index.Mapping().(*mapping.IndexMappingImpl)
	if !ok {
		t.Fatalf("unexpected mapping type %T", b.index.Mapping())
	}
	return im.ScoringModel
}

func TestBleveNewIndexScoresWithBM25(t *testing.T) {
	// A freshly created index ranks with BM25, not bleve's default TF-IDF.
	idx, _ := testBleveIndex(t)
	if got := scoringModelOf(t, idx); got != index.BM25Scoring {
		t.Fatalf("scoring model = %q, want %q", got, index.BM25Scoring)
	}
}

func TestBleveReopenRebuildsTFIDFIndexAsBM25(t *testing.T) {
	// An index created before the BM25 switch stores the TF-IDF mapping on
	// disk; opening it must replace it with a BM25 index, or the switch never
	// reaches existing installs. Its documents are dropped (startup re-fills
	// files, conversations and todos), so an old doc must be gone.
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, "memory.bleve")

	old := buildBleveMapping().(*mapping.IndexMappingImpl)
	old.ScoringModel = "" // bleve default = TF-IDF, as every pre-switch index was built
	raw, err := bleve.New(indexPath, old)
	if err != nil {
		t.Fatalf("create old index: %v", err)
	}
	if err := raw.Index("conversation:old:1", map[string]interface{}{"content": "stalemarker", "source": "conversation"}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	idx, err := NewBleveIndex(indexPath, map[string]SourceConfig{"memory": {Dir: memDir, Weight: 1}}, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer idx.Close()
	if got := scoringModelOf(t, idx); got != index.BM25Scoring {
		t.Fatalf("scoring model after reopen = %q, want %q", got, index.BM25Scoring)
	}
	if n, _ := idx.index.DocCount(); n != 0 {
		t.Fatalf("rebuilt index holds %d docs, want 0 (old TF-IDF docs carried over)", n)
	}
}

func TestBleveReopenKeepsBM25Index(t *testing.T) {
	// Control: an index that is already BM25 is reopened as-is, not rebuilt.
	idx, memDir := testBleveIndex(t)
	if err := os.WriteFile(filepath.Join(memDir, "keep.md"), []byte("keepmarker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Reindex(); err != nil {
		t.Fatal(err)
	}
	path := idx.indexPath
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	idx2, err := NewBleveIndex(path, map[string]SourceConfig{"memory": {Dir: memDir, Weight: 1}}, 0, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	if n, _ := idx2.index.DocCount(); n == 0 {
		t.Fatal("BM25 index was rebuilt on reopen; its documents should persist")
	}
}

func TestBleveLongMemoryFileOutranksShortOneWordMessages(t *testing.T) {
	// #2234 repro shape: one long memory file contains BOTH query words; many
	// short chat messages contain only one. The file must rank first. Under
	// TF-IDF, and under BM25 with length normalisation (b>0), the short
	// messages buried it because a ~3,000-word file is hundreds of times the
	// average document length.
	idx, memDir := testBleveIndex(t)
	idx.conversationWeight = 1 // isolate scoring from the source weighting
	filler := strings.Repeat("lorem ipsum dolor sit amet consectetur adipiscing elit ", 400)
	body := filler + " the quokkaword spreadsheet plot lives in tools " + filler
	if err := os.WriteFile(filepath.Join(memDir, "long.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Reindex(); err != nil {
		t.Fatal(err)
	}
	// Both words are COMMON (each in 30 messages), as "todo" and "chart" are in
	// real chat history; a rare second word would carry the file on IDF alone
	// and the test could not fail.
	for i := 0; i < 30; i++ {
		idx.IndexConversation(fmt.Sprintf("how many quokkaword %d", i), "s", int64(2*i+1))
		idx.IndexConversation(fmt.Sprintf("nice plot %d", i), "s", int64(2*i+2))
	}
	res, err := idx.Search("quokkaword plot", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || res[0].Source == "conversation" {
		top := "none"
		if len(res) > 0 {
			top = res[0].Source + " " + res[0].Path
		}
		t.Fatalf("top hit = %s; want the long memory file (long.md) that has both words", top)
	}
}
