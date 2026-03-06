package secrets

import (
	"strings"
	"testing"
)

// TestGeneratePassphrase verifies passphrase generation with valid words from EFF list.
func TestGeneratePassphrase(t *testing.T) {
	p, err := GeneratePassphrase(5)
	if err != nil {
		t.Fatalf("GeneratePassphrase(5): %v", err)
	}

	words := strings.Split(p, "-")
	if len(words) != 5 {
		t.Errorf("expected 5 words, got %d: %q", len(words), p)
	}

	wordSet := make(map[string]bool, len(effShortWordlist))
	for _, w := range effShortWordlist {
		wordSet[w] = true
	}
	for _, w := range words {
		if !wordSet[w] {
			t.Errorf("word %q not in EFF wordlist", w)
		}
	}

	p1, err := GeneratePassphrase(1)
	if err != nil {
		t.Fatalf("GeneratePassphrase(1): %v", err)
	}
	if strings.Contains(p1, "-") {
		t.Errorf("single word should have no hyphens: %q", p1)
	}
	if !wordSet[p1] {
		t.Errorf("single word %q not in wordlist", p1)
	}

	if _, err := GeneratePassphrase(0); err == nil {
		t.Error("expected error for wordCount=0")
	}
	if _, err := GeneratePassphrase(-1); err == nil {
		t.Error("expected error for wordCount=-1")
	}

	p2, _ := GeneratePassphrase(5)
	p3, _ := GeneratePassphrase(5)
	if p2 == p3 {
		t.Errorf("two consecutive passphrases are identical: %q (extremely unlikely)", p2)
	}
}

// TestEFFWordlistSize verifies the wordlist has exactly 1296 words.
func TestEFFWordlistSize(t *testing.T) {
	if len(effShortWordlist) != 1296 {
		t.Errorf("EFF short wordlist has %d words, expected 1296", len(effShortWordlist))
	}
}

// TestGeneratePassphraseWordCount verifies wordCount parameter is honored.
func TestGeneratePassphraseWordCount(t *testing.T) {
	for count := 2; count <= 10; count++ {
		p, err := GeneratePassphrase(count)
		if err != nil {
			t.Errorf("GeneratePassphrase(%d): %v", count, err)
			continue
		}
		words := strings.Split(p, "-")
		if len(words) != count {
			t.Errorf("wordCount=%d: got %d words in %q", count, len(words), p)
		}
	}
}

// TestGeneratePassphraseZeroError verifies error for invalid word count.
func TestGeneratePassphraseZeroError(t *testing.T) {
	_, err := GeneratePassphrase(0)
	if err == nil {
		t.Error("expected error for wordCount=0")
	}
	_, err = GeneratePassphrase(-5)
	if err == nil {
		t.Error("expected error for negative wordCount")
	}
}
