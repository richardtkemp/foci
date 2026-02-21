package anthropic_test

import (
	"clod/anthropic"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestCacheSharing is the go/no-go test for the entire project.
// It validates that branched sessions share cache prefixes with parent sessions.
//
// Flow:
//  1. Send a request with system prompt + prefix messages → cache WRITE
//  2. Send another request on same session (same prefix, new msgs) → cache READ
//  3. Send a request on a branch (same prefix, different new msgs) → cache READ (KEY TEST)
//  4. Send another request on parent → cache READ still works
func TestCacheSharing(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	client := anthropic.NewClient(apiKey)
	ctx := context.Background()

	// Build a large system prompt. Haiku requires >= 2048 tokens for caching.
	// ~100 lines of unique text gets us well over that threshold.
	var sb strings.Builder
	sb.WriteString("You are a knowledgeable AI assistant specializing in programming and technology.\n\n")
	for i := range 100 {
		fmt.Fprintf(&sb, "Knowledge domain %d: You have expertise in area %d, covering subtopics %d-alpha, %d-beta, and %d-gamma. When asked about domain %d, provide comprehensive explanations with practical examples and best practices.\n", i, i, i, i, i, i)
	}

	system := []anthropic.SystemBlock{{
		Type:         "text",
		Text:         sb.String(),
		CacheControl: anthropic.Ephemeral(),
	}}

	// Shared prefix: pre-built conversation that parent and branch both share.
	// cache_control on the last message marks the cache breakpoint.
	prefix := []anthropic.Message{
		{Role: "user", Content: anthropic.TextContent("Tell me about the Go programming language and its key features.")},
		{Role: "assistant", Content: anthropic.TextContent("Go is a statically typed, compiled programming language designed at Google by Robert Griesemer, Rob Pike, and Ken Thompson. Key features include simplicity, fast compilation, built-in concurrency with goroutines and channels, garbage collection, and a comprehensive standard library. Go emphasizes readability and maintainability, with a deliberately small language specification.")},
		{Role: "user", Content: anthropic.TextContent("How do goroutines work compared to OS threads?")},
		{Role: "assistant", Content: anthropic.CachedTextContent("Goroutines are lightweight threads managed by the Go runtime rather than the operating system. They start with a small stack of a few kilobytes that grows and shrinks as needed, unlike OS threads which typically allocate one to eight megabytes of fixed stack space. This means you can easily run thousands or even millions of goroutines in a single program. The Go scheduler uses an M:N model, multiplexing many goroutines onto a smaller number of OS threads. Goroutines communicate through channels, following the communicating sequential processes model, which makes concurrent programming safer and more structured than shared-memory approaches with explicit locks.")},
	}

	model := "claude-haiku-4-5"
	maxTokens := 128

	// --- Step 1: First parent request (expect cache WRITE) ---
	t.Log("=== Step 1: First parent request (expect cache WRITE) ===")
	resp1, err := client.SendMessage(ctx, &anthropic.MessageRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    system,
		Messages: withNewUserMessage(prefix, "How does error handling work in Go?"),
	})
	if err != nil {
		t.Fatalf("Step 1 failed: %v", err)
	}
	logUsage(t, "Step 1", resp1.Usage)
	if resp1.Usage.CacheCreationInputTokens == 0 {
		t.Error("Step 1: expected cache_creation_input_tokens > 0 (cache write)")
	}

	// --- Step 2: Second parent request (expect cache READ) ---
	t.Log("=== Step 2: Second parent request (expect cache READ) ===")
	parentMsgs := appendMessages(prefix,
		anthropic.Message{Role: "user", Content: anthropic.TextContent("How does error handling work in Go?")},
		anthropic.Message{Role: "assistant", Content: resp1.Content},
		anthropic.Message{Role: "user", Content: anthropic.TextContent("Tell me about Go interfaces.")},
	)
	resp2, err := client.SendMessage(ctx, &anthropic.MessageRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  parentMsgs,
	})
	if err != nil {
		t.Fatalf("Step 2 failed: %v", err)
	}
	logUsage(t, "Step 2", resp2.Usage)
	if resp2.Usage.CacheReadInputTokens == 0 {
		t.Error("Step 2: expected cache_read_input_tokens > 0 (cache read)")
	}

	// --- Step 3: Branch request (expect cache READ on shared prefix) ---
	// This is THE critical test. The branch has the same system prompt and
	// prefix messages as the parent, but diverges with a different question.
	t.Log("=== Step 3: BRANCH request (expect cache READ on shared prefix) ===")
	resp3, err := client.SendMessage(ctx, &anthropic.MessageRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    system,
		Messages: withNewUserMessage(prefix, "What is the Go module system and how does dependency management work?"),
	})
	if err != nil {
		t.Fatalf("Step 3 failed: %v", err)
	}
	logUsage(t, "Step 3", resp3.Usage)
	if resp3.Usage.CacheReadInputTokens == 0 {
		t.Fatal("Step 3: CRITICAL FAILURE — expected cache_read_input_tokens > 0 on branch, got 0. Branch cache sharing does NOT work. Project is a no-go.")
	}
	t.Log("Step 3: SUCCESS — branch shares cache prefix with parent!")

	// --- Step 4: Parent after branch (expect cache READ still works) ---
	t.Log("=== Step 4: Parent after branch (expect cache READ still works) ===")
	parentMsgs2 := appendMessages(prefix,
		anthropic.Message{Role: "user", Content: anthropic.TextContent("How does error handling work in Go?")},
		anthropic.Message{Role: "assistant", Content: resp1.Content},
		anthropic.Message{Role: "user", Content: anthropic.TextContent("Tell me about Go interfaces.")},
		anthropic.Message{Role: "assistant", Content: resp2.Content},
		anthropic.Message{Role: "user", Content: anthropic.TextContent("What about Go's type system and generics?")},
	)
	resp4, err := client.SendMessage(ctx, &anthropic.MessageRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  parentMsgs2,
	})
	if err != nil {
		t.Fatalf("Step 4 failed: %v", err)
	}
	logUsage(t, "Step 4", resp4.Usage)
	if resp4.Usage.CacheReadInputTokens == 0 {
		t.Error("Step 4: expected cache_read_input_tokens > 0 (parent cache still works after branch)")
	}

	t.Log("=== ALL STEPS PASSED — Cache sharing works. Project is a go. ===")
}

// withNewUserMessage clones prefix and appends a new user message.
func withNewUserMessage(prefix []anthropic.Message, text string) []anthropic.Message {
	msgs := make([]anthropic.Message, len(prefix), len(prefix)+1)
	copy(msgs, prefix)
	return append(msgs, anthropic.Message{Role: "user", Content: anthropic.TextContent(text)})
}

// appendMessages clones prefix and appends additional messages.
func appendMessages(prefix []anthropic.Message, extra ...anthropic.Message) []anthropic.Message {
	msgs := make([]anthropic.Message, len(prefix), len(prefix)+len(extra))
	copy(msgs, prefix)
	return append(msgs, extra...)
}

func logUsage(t *testing.T, step string, u anthropic.Usage) {
	t.Logf("%s — input: %d, output: %d, cache_creation: %d, cache_read: %d",
		step, u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens)
}
