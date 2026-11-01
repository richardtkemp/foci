package agentrename

import (
	"fmt"
	"strings"
)

// unifiedDiff returns a unified diff of a → b (line based, longest-common-
// subsequence) with ctx lines of context, labelled name. "" when equal.
// ctx = 0 shows only the changed lines (used for secrets.toml, whose context
// lines may hold secret values).
func unifiedDiff(name, a, b string, ctx int) string {
	if a == b {
		return ""
	}
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	ops := diffOps(al, bl)

	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)
	// Group ops into hunks: changed runs plus ctx lines of context.
	n := len(ops)
	for i := 0; i < n; {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i - ctx
		if start < 0 {
			start = 0
		}
		end := i
		for end < n {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			// a run of context: does another change follow within 2*ctx?
			j := end
			for j < n && ops[j].kind == ' ' {
				j++
			}
			if j < n && j-end <= 2*ctx {
				end = j
				continue
			}
			break
		}
		stop := end + ctx
		if stop > n {
			stop = n
		}
		aStart, bStart, aLen, bLen := ops[start].ai, ops[start].bi, 0, 0
		for _, o := range ops[start:stop] {
			if o.kind != '+' {
				aLen++
			}
			if o.kind != '-' {
				bLen++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
		for _, o := range ops[start:stop] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		i = stop
	}
	return out.String()
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

type diffOp struct {
	kind   byte // ' ', '-', '+'
	text   string
	ai, bi int // 0-based position in a and b where this op sits
}

func diffOps(a, b []string) []diffOp {
	// lcs[i][j] = LCS length of a[i:], b[j:]
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i], i, j})
			i++
			j++
		case j < len(b) && (i == len(a) || lcs[i][j+1] > lcs[i+1][j]):
			ops = append(ops, diffOp{'+', b[j], i, j})
			j++
		default:
			ops = append(ops, diffOp{'-', a[i], i, j})
			i++
		}
	}
	return ops
}
