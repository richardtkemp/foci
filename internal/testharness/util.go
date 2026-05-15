// Small helpers shared across testharness files. Kept separate so the
// big files (telegram.go, gateway.go) stay focused.

package testharness

import (
	"net/url"
	"strconv"
)

// parseURLForm wraps url.ParseQuery to keep error handling uniform.
// Returns an empty Values on error so callers can chain Get without nil checks.
func parseURLForm(body []byte) (url.Values, error) {
	if len(body) == 0 {
		return url.Values{}, nil
	}
	return url.ParseQuery(string(body))
}

// parseInt64 parses a decimal int64 from a string, returning 0 on error.
// Suited for Telegram chat_id / message_id parsing where 0 is a safe sentinel.
func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// formatInt64 is strconv.FormatInt with a default base for use in the
// json/form extractor helper above.
func formatInt64(n int64) string {
	return strconv.FormatInt(n, 10)
}
