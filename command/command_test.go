package command

import (
	"context"
	"fmt"
	"testing"
)

func TestRegistryDispatch(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:        "test",
		Description: "test command",
		Execute: func(ctx context.Context, args string) (string, error) {
			if args == "" {
				return "no args", nil
			}
			return "args: " + args, nil
		},
	})

	ctx := context.Background()

	// Basic dispatch
	result, ok := r.Dispatch(ctx, "/test")
	if !ok {
		t.Fatal("expected command to be found")
	}
	if result != "no args" {
		t.Errorf("result = %q", result)
	}

	// With args
	result, ok = r.Dispatch(ctx, "/test hello world")
	if !ok {
		t.Fatal("expected command to be found")
	}
	if result != "args: hello world" {
		t.Errorf("result = %q", result)
	}

	// Unknown command
	_, ok = r.Dispatch(ctx, "/unknown")
	if ok {
		t.Error("expected unknown command to return false")
	}

	// Not a command
	_, ok = r.Dispatch(ctx, "regular message")
	if ok {
		t.Error("expected non-command to return false")
	}
}

func TestDispatchCaseInsensitive(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:    "ping",
		Execute: func(ctx context.Context, args string) (string, error) { return "pong", nil },
	})

	result, ok := r.Dispatch(context.Background(), "/PING")
	if !ok {
		t.Fatal("expected case-insensitive match")
	}
	if result != "pong" {
		t.Errorf("result = %q", result)
	}
}

func TestDispatchError(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name: "fail",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "", fmt.Errorf("something broke")
		},
	})

	result, ok := r.Dispatch(context.Background(), "/fail")
	if !ok {
		t.Fatal("expected command to be found")
	}
	if result != "Error: something broke" {
		t.Errorf("result = %q", result)
	}
}

func TestAll(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{Name: "beta"})
	r.Register(&Command{Name: "alpha"})

	all := r.All()
	if len(all) != 2 {
		t.Fatalf("got %d commands", len(all))
	}
	if all[0].Name != "alpha" {
		t.Errorf("first = %s, want alpha (sorted)", all[0].Name)
	}
}
