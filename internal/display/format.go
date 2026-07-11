package display

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FormatDuration formats a duration as compact human-readable text.
// Examples: "38s", "3m12s", "3h12m", "2d4h".
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd%dh", days, hours)
}

// FormatCommas formats an integer with comma separators (e.g. 32793 → "32,793").
func FormatCommas(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + FormatCommas(-n)
	}
	if len(s) <= 3 {
		return s
	}
	var result strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result.WriteByte(',')
		}
		result.WriteRune(c)
	}
	return result.String()
}

// FormatTokensAbbrev formats a token count compactly for status displays:
// 1_000_000 -> "1M", 1_500_000 -> "1.5M", 374_686 -> "375k", 512 -> "512".
// Base-1000 units (k, M): one decimal for millions (trailing ".0" trimmed),
// nearest whole thousand for k, raw below 1000. Keeps context/quota readouts
// glanceable instead of showing full comma-grouped counts (#1149).
func FormatTokensAbbrev(n int) string {
	if n < 0 {
		return "-" + FormatTokensAbbrev(-n)
	}
	switch {
	case n >= 1_000_000:
		s := strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64)
		return strings.TrimSuffix(s, ".0") + "M"
	case n >= 1_000:
		// Round to nearest thousand; promote to M at the 1000k boundary
		// (e.g. 999_600 -> "1M", not "1000k").
		k := (n + 500) / 1000
		if k >= 1000 {
			return FormatTokensAbbrev(k * 1000)
		}
		return strconv.Itoa(k) + "k"
	default:
		return strconv.Itoa(n)
	}
}

// FormatBytes formats a byte count as human-readable (e.g. "1.5 KB").
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// CompactRelativeTime formats a timestamp as a compact relative time string
// without the " ago" suffix (e.g. "3h", "18m", "now"). Suitable for table columns.
func CompactRelativeTime(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// RelativeTime formats a timestamp as a relative time string (e.g. "3h ago").
func RelativeTime(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		m := int(d.Minutes())
		if m == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", m)
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1d ago"
	}
	return fmt.Sprintf("%dd ago", days)
}
