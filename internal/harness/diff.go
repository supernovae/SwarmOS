package harness

import (
	"fmt"
	"strings"
)

// maxDiffCells bounds the line-comparison table so a preview of a very large
// file stays cheap; beyond it the diff only notes that the file differs.
const maxDiffCells = 4 << 20

const diffContext = 3

type diffOp struct {
	kind byte // ' ', '-', or '+'
	text string
}

// unifiedDiff renders the change from before to after as a unified diff of rel.
func unifiedDiff(rel string, before, after []byte) string {
	a, b := splitLines(string(before)), splitLines(string(after))
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", rel, rel)
	ops, ok := diffLines(a, b)
	if !ok {
		out.WriteString("@@ file too large to diff; compare it by hand @@\n")
		return out.String()
	}
	// aAt[k] and bAt[k] count the lines of each side that precede ops[k].
	aAt, bAt := make([]int, len(ops)+1), make([]int, len(ops)+1)
	for k, op := range ops {
		aAt[k+1], bAt[k+1] = aAt[k], bAt[k]
		if op.kind != '+' {
			aAt[k+1]++
		}
		if op.kind != '-' {
			bAt[k+1]++
		}
	}
	for i := 0; i < len(ops); {
		for i < len(ops) && ops[i].kind == ' ' {
			i++
		}
		if i == len(ops) {
			break
		}
		last := i
		for k := i; k < len(ops); k++ {
			if ops[k].kind == ' ' {
				continue
			}
			if k-last > 2*diffContext {
				break
			}
			last = k
		}
		start, end := max(0, i-diffContext), min(len(ops), last+diffContext+1)
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aAt[start], aAt[end]-aAt[start]), hunkRange(bAt[start], bAt[end]-bAt[start]))
		for _, op := range ops[start:end] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

func hunkRange(before, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", before)
	}
	if count == 1 {
		return fmt.Sprintf("%d", before+1)
	}
	return fmt.Sprintf("%d,%d", before+1, count)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffLines aligns a and b on a longest common subsequence of lines.
func diffLines(a, b []string) ([]diffOp, bool) {
	var ops []diffOp
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		ops = append(ops, diffOp{' ', a[0]})
		a, b = a[1:], b[1:]
	}
	var tail []diffOp
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		tail = append(tail, diffOp{' ', a[len(a)-1]})
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	n, m := len(a), len(b)
	// (n+1)*(m+1) > maxDiffCells, written so it cannot overflow.
	if n >= maxDiffCells || m >= maxDiffCells || m+1 > maxDiffCells/(n+1) {
		return nil, false
	}
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	for k := len(tail) - 1; k >= 0; k-- {
		ops = append(ops, tail[k])
	}
	return ops, true
}
