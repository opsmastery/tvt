package propose

import (
	"bytes"
	"fmt"
	"strings"
)

// Diff is a unified diff of one file, from old to new, with git's a/ and
// b/ headers, so `git apply` takes it (tvt propose --output patch). Equal
// inputs give an empty diff. path is the file's path in its repository.
func Diff(path string, old, new []byte) string {
	if bytes.Equal(old, new) {
		return ""
	}
	a, b := splitLines(old), splitLines(new)
	ops := lineOps(a, b)
	var out strings.Builder
	fmt.Fprintf(&out, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", path, path, path, path)
	const context = 3
	// Walk the ops, cutting hunks around each run of changes.
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-context, 0)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			// A run of equal lines longer than two contexts ends the hunk.
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				end = min(end+context, len(ops))
				break
			}
			end = run
		}
		writeHunk(&out, ops[start:end])
		i = end
	}
	return out.String()
}

type lineOp struct {
	kind   byte // ' ', '-' or '+'
	line   string
	ai, bi int // 1-based line numbers in a and b (the next line, for an insert or delete)
}

func writeHunk(w *strings.Builder, ops []lineOp) {
	aStart, bStart, aN, bN := ops[0].ai, ops[0].bi, 0, 0
	for _, o := range ops {
		if o.kind != '+' {
			aN++
		}
		if o.kind != '-' {
			bN++
		}
	}
	if aN == 0 {
		aStart--
	}
	if bN == 0 {
		bStart--
	}
	fmt.Fprintf(w, "@@ -%d,%d +%d,%d @@\n", aStart, aN, bStart, bN)
	for _, o := range ops {
		w.WriteByte(o.kind)
		w.WriteString(o.line)
		if !strings.HasSuffix(o.line, "\n") {
			w.WriteString("\n\\ No newline at end of file\n")
		}
	}
}

// splitLines splits after each newline; a last line without one is kept.
func splitLines(b []byte) []string {
	var out []string
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, string(b))
			break
		}
		out = append(out, string(b[:i+1]))
		b = b[i+1:]
	}
	return out
}

// lineOps aligns a and b: the common prefix and suffix, then a longest
// common subsequence of the middle (config edits are small, so the middle
// is too).
func lineOps(a, b []string) []lineOp {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	am, bm := a[p:len(a)-s], b[p:len(b)-s]
	// lcs[i][j]: LCS length of am[i:] and bm[j:].
	lcs := make([][]int, len(am)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(bm)+1)
	}
	for i := len(am) - 1; i >= 0; i-- {
		for j := len(bm) - 1; j >= 0; j-- {
			if am[i] == bm[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []lineOp
	ai, bi := 1, 1
	eq := func(l string) { ops = append(ops, lineOp{' ', l, ai, bi}); ai++; bi++ }
	for _, l := range a[:p] {
		eq(l)
	}
	i, j := 0, 0
	for i < len(am) || j < len(bm) {
		switch {
		case i < len(am) && j < len(bm) && am[i] == bm[j]:
			eq(am[i])
			i, j = i+1, j+1
		case j < len(bm) && (i == len(am) || lcs[i][j+1] >= lcs[i+1][j]):
			ops = append(ops, lineOp{'+', bm[j], ai, bi})
			bi++
			j++
		default:
			ops = append(ops, lineOp{'-', am[i], ai, bi})
			ai++
			i++
		}
	}
	for _, l := range a[len(a)-s:] {
		eq(l)
	}
	return ops
}
