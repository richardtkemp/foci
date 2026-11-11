package voice

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Spoken blocks (#3050). A voice-mode bubble is read aloud from its markdown
// source. A mermaid diagram, a table or a code block does not read aloud: its
// source is noise. SpeakBlocks puts a short spoken sentence in each one's
// place, written by a BlockSummariser (a fast model), or the block's marker
// when the summariser fails, is off, or is slower than the bound. The source of
// a block is never left in the text. Prose around the blocks is kept as it is,
// in order. Formulas ($$) are not blocks here: they read as before.

// BlockKind is the kind of a block that does not read aloud.
type BlockKind string

const (
	BlockDiagram BlockKind = "diagram" // a fenced block with the mermaid language
	BlockTable   BlockKind = "table"   // a markdown (GFM) table
	BlockCode    BlockKind = "code"    // any other fenced code block
)

// Block is one block that does not read aloud.
type Block struct {
	Kind   BlockKind
	Lang   string // the fence's info word ("" for a table or a bare fence)
	Source string // the block's text: the fence body, or the table's lines
}

// BlockMarker is what is spoken for a block of kind k when no summary is
// available: the same words the app's Speak item uses (#2858).
func BlockMarker(k BlockKind) string {
	switch k {
	case BlockDiagram:
		return "Diagram omitted."
	case BlockTable:
		return "Table omitted."
	default:
		return "Code omitted."
	}
}

// BlockSummariser returns one spoken sentence for b. An error or "" means no
// summary: the block's marker is spoken.
type BlockSummariser func(ctx context.Context, b Block) (string, error)

// segment is a run of prose (block == nil) or one block.
type segment struct {
	prose string
	block *Block
}

var (
	// fenceOpenRE matches a fence line: up to three spaces, three or more
	// backticks or tildes, then an optional info word.
	fenceOpenRE = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*([^ \t`]*)")
	// tableDelimRE matches a GFM table delimiter row: cells of dashes with
	// optional alignment colons, separated by pipes.
	tableDelimRE = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)
)

// splitSpeechSegments splits markdown text into prose and blocks, in order.
func splitSpeechSegments(text string) []segment {
	lines := strings.Split(text, "\n")
	var out []segment
	var prose []string
	flush := func() {
		if len(prose) > 0 {
			out = append(out, segment{prose: strings.Join(prose, "\n")})
			prose = nil
		}
	}
	for i := 0; i < len(lines); {
		line := lines[i]
		if m := fenceOpenRE.FindStringSubmatch(line); m != nil {
			fence, lang := m[1], m[2]
			j := i + 1
			for j < len(lines) && !closesFence(lines[j], fence) {
				j++
			}
			kind := BlockCode
			if strings.EqualFold(lang, "mermaid") {
				kind = BlockDiagram
			}
			flush()
			out = append(out, segment{block: &Block{Kind: kind, Lang: lang, Source: strings.Join(lines[i+1:min(j, len(lines))], "\n")}})
			i = j + 1 // past the closing fence (or the end, for an unclosed one)
			continue
		}
		// A table: a header row with a pipe, then a delimiter row with a pipe
		// (a bare "---" under a line is a heading or a rule, not a table).
		if strings.Contains(line, "|") && i+1 < len(lines) && strings.Contains(lines[i+1], "|") && tableDelimRE.MatchString(lines[i+1]) {
			j := i + 2
			for j < len(lines) && strings.TrimSpace(lines[j]) != "" && strings.Contains(lines[j], "|") {
				j++
			}
			flush()
			out = append(out, segment{block: &Block{Kind: BlockTable, Source: strings.Join(lines[i:j], "\n")}})
			i = j
			continue
		}
		prose = append(prose, line)
		i++
	}
	flush()
	return out
}

// closesFence reports whether line closes a fence opened with fence: the same
// character, at least as many, and nothing else but spaces.
func closesFence(line, fence string) bool {
	t := strings.TrimSpace(line)
	if len(t) < len(fence) || len(line)-len(strings.TrimLeft(line, " ")) > 3 {
		return false
	}
	return strings.Trim(t, fence[:1]) == ""
}

// SpeakBlocks returns text with each diagram, table and code block replaced by
// a spoken sentence: summarise's answer, or the block's marker (BlockMarker)
// when summarise is nil, fails, answers "", or has not answered within bound
// (0 = only ctx bounds it). The blocks are summarised at the same time. Text
// with no block is returned unchanged, with no call.
func SpeakBlocks(ctx context.Context, text string, summarise BlockSummariser, bound time.Duration) string {
	segs := splitSpeechSegments(text)
	var blocks []int
	for i, s := range segs {
		if s.block != nil {
			blocks = append(blocks, i)
		}
	}
	if len(blocks) == 0 {
		return text
	}
	spoken := make([]string, len(segs))
	if summarise != nil {
		callCtx := ctx
		var cancel context.CancelFunc
		if bound > 0 {
			callCtx, cancel = context.WithTimeout(ctx, bound)
		} else {
			callCtx, cancel = context.WithCancel(ctx)
		}
		var mu sync.Mutex
		got := make([]string, len(segs)) // written by the calls, under mu
		var wg sync.WaitGroup
		for _, i := range blocks {
			b := *segs[i].block
			if strings.TrimSpace(b.Source) == "" {
				continue
			}
			wg.Add(1)
			go func(i int, b Block) {
				defer wg.Done()
				s, err := summarise(callCtx, b)
				s = strings.TrimSpace(s)
				if err != nil || s == "" || callCtx.Err() != nil {
					return
				}
				mu.Lock()
				got[i] = s
				mu.Unlock()
			}(i, b)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		// A summariser that ignores its context cannot hold the bubble past
		// the bound: its late answer is dropped.
		select {
		case <-done:
		case <-callCtx.Done():
		}
		cancel()
		mu.Lock()
		copy(spoken, got)
		mu.Unlock()
	}
	parts := make([]string, 0, len(segs))
	for i, s := range segs {
		if s.block == nil {
			parts = append(parts, s.prose)
			continue
		}
		say := spoken[i]
		if say == "" {
			say = BlockMarker(s.block.Kind)
		} else if !strings.ContainsAny(say[len(say)-1:], ".!?") {
			say += "."
		}
		parts = append(parts, say)
	}
	return strings.Join(parts, "\n")
}
