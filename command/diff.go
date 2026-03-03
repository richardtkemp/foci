package command

import (
	"fmt"
	"strings"
)

// diffLines produces a unified-format diff between texts a and b.
// labelA and labelB are used in the --- / +++ headers.
// Returns "" if the texts are identical.
func diffLines(a, b, labelA, labelB string) string {
	aLines := diffSplitLines(a)
	bLines := diffSplitLines(b)

	ops := lcsEditScript(aLines, bLines)

	// Count changes
	changes := 0
	for _, o := range ops {
		if o.op != ' ' {
			changes++
		}
	}
	if changes == 0 {
		return ""
	}

	const ctx = 3

	// Mark which ops are within ctx lines of a change
	n := len(ops)
	include := make([]bool, n)
	for i, o := range ops {
		if o.op != ' ' {
			for j := max(0, i-ctx); j <= min(n-1, i+ctx); j++ {
				include[j] = true
			}
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", labelA, labelB)

	// Walk through, emitting hunks for contiguous included regions
	i := 0
	for i < n {
		if !include[i] {
			i++
			continue
		}
		// Find contiguous included region
		hStart := i
		for i < n && include[i] {
			i++
		}
		hEnd := i

		// Compute a-line and b-line at hStart by counting prior ops
		aLine, bLine := 1, 1
		for j := 0; j < hStart; j++ {
			if ops[j].op == ' ' || ops[j].op == '-' {
				aLine++
			}
			if ops[j].op == ' ' || ops[j].op == '+' {
				bLine++
			}
		}

		// Count a-lines and b-lines in this hunk
		aCount, bCount := 0, 0
		for j := hStart; j < hEnd; j++ {
			if ops[j].op == ' ' || ops[j].op == '-' {
				aCount++
			}
			if ops[j].op == ' ' || ops[j].op == '+' {
				bCount++
			}
		}

		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aLine, aCount, bLine, bCount)
		for j := hStart; j < hEnd; j++ {
			fmt.Fprintf(&sb, "%c%s\n", ops[j].op, ops[j].line)
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// diffChangedLines counts the number of added/removed lines in a unified diff string.
func diffChangedLines(diff string) int {
	count := 0
	for _, line := range strings.Split(diff, "\n") {
		if len(line) == 0 {
			continue
		}
		if (line[0] == '+' || line[0] == '-') && !strings.HasPrefix(line, "---") && !strings.HasPrefix(line, "+++") {
			count++
		}
	}
	return count
}

func diffSplitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type editOp struct {
	op   byte // ' ', '-', '+'
	line string
}

func lcsEditScript(a, b []string) []editOp {
	m, n := len(a), len(b)

	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack
	var ops []editOp
	i, j := m, n
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && a[i-1] == b[j-1] {
			ops = append(ops, editOp{' ', a[i-1]})
			i--
			j--
		} else if j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]) {
			ops = append(ops, editOp{'+', b[j-1]})
			j--
		} else {
			ops = append(ops, editOp{'-', a[i-1]})
			i--
		}
	}

	// Reverse
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	return ops
}
