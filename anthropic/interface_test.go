package anthropic_test

import (
	"foci/anthropic"
	"foci/provider"
)

// Compile-time check: *anthropic.Client must implement provider.Client.
var _ provider.Client = (*anthropic.Client)(nil)
