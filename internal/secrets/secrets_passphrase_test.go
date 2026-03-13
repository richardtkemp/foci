package secrets

import (
	"strings"
	"testing"
)

func TestGeneratePassphrase(t *testing.T) {
	// Proves the full contract of GeneratePassphrase: it produces
	// the requested number of words joined by hyphens, every word comes from the EFF
	// wordlist, consecutive calls produce different passphrases, and invalid counts error.
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

func TestEFFWordlistSize(t *testing.T) {
	// Proves the embedded EFF short wordlist contains exactly 1296
	// words, matching the official EFF specification for 4-dice (6^4) lookups.
	if len(effShortWordlist) != 1296 {
		t.Errorf("EFF short wordlist has %d words, expected 1296", len(effShortWordlist))
	}
}

func TestGeneratePassphraseWordCount(t *testing.T) {
	// Proves that the wordCount parameter is strictly
	// honored across a range of values, with the output containing exactly that many
	// hyphen-separated words.
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

