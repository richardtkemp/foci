package anthropic_test

import (
	"foci/internal/anthropic"
	"foci/internal/provider"
)

// Compile-time check: *anthropic.Client must implement provider.Client.
var _ provider.Client = (*anthropic.Client)(nil)
