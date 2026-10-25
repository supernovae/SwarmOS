package cli

import (
	"fmt"
	"strings"
)

// editMaxDiffCells bounds the alignment table. Larger changes use exact replacement hunks.
const editMaxDiffCells = 4 << 20

const editDiffContext = 3

type editDiffOp struct {
	kind byte // ' ', '-', or '+'
	text string
}

// issueEditDiff renders the complete body change as a unified diff.
func issueEditDiff(before, after string) string {
	if before == after {
		return ""
	}
	a, b := editSplitLines(before), editSplitLines(after)
	var out strings.Builder
	out.WriteString("--- a/body\n+++ b/body\n")
	ops, ok := editDiffLines(a, b)
	if !ok {
		for _, line := range a {
			ops = append(ops, editDiffOp{'-', line})
		}
		for _, line := range b {
			ops = append(ops, editDiffOp{'+', line})
		}
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
			if k-last > 2*editDiffContext {
				break
			}
			last = k
		}
		start, end := max(0, i-editDiffContext), min(len(ops), last+editDiffContext+1)
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", editHunkRange(aAt[start], aAt[end]-aAt[start]), editHunkRange(bAt[start], bAt[end]-bAt[start]))
		for _, op := range ops[start:end] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			if !strings.HasSuffix(op.text, "\n") {
				out.WriteString("\n\\ No newline at end of file\n")
			}
		}
		i = end
	}
	return out.String()
}

func editHunkRange(before, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", before)
	}
	if count == 1 {
		return fmt.Sprintf("%d", before+1)
	}
	return fmt.Sprintf("%d,%d", before+1, count)
}

func editSplitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// editDiffLines aligns a and b on a longest common subsequence of lines.
func editDiffLines(a, b []string) ([]editDiffOp, bool) {
	var ops []editDiffOp
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		ops = append(ops, editDiffOp{' ', a[0]})
		a, b = a[1:], b[1:]
	}
	var tail []editDiffOp
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		tail = append(tail, editDiffOp{' ', a[len(a)-1]})
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	n, m := len(a), len(b)
	if n > 0 && m > editMaxDiffCells/n {
		return nil, false
	}
	// lcs[i][j] is the common subsequence length of a[i:] and b[j:]; at
	// treats the row and column past either end as zero.
	lcs := make([][]int32, n)
	for i := range lcs {
		lcs[i] = make([]int32, m)
	}
	at := func(i, j int) int32 {
		if i == n || j == m {
			return 0
		}
		return lcs[i][j]
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = at(i+1, j+1) + 1
			} else {
				lcs[i][j] = max(at(i+1, j), at(i, j+1))
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, editDiffOp{' ', a[i]})
			i, j = i+1, j+1
		case at(i+1, j) >= at(i, j+1):
			ops = append(ops, editDiffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, editDiffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, editDiffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, editDiffOp{'+', b[j]})
	}
	for k := len(tail) - 1; k >= 0; k-- {
		ops = append(ops, tail[k])
	}
	return ops, true
}
