package wikitext

import (
	"fmt"
	"strings"
)

// maxDiffCells bounds the LCS table. Common prefix and suffix are trimmed
// first, so this only bites when two large revisions differ throughout.
const maxDiffCells = 16 << 20

// UnifiedDiff returns a unified diff of two documents with the given lines
// of context. It is line-based and exact (an LCS), which is what an agent
// reviewing a revision needs; it is not tuned for huge rewrites, which it
// reports as a full replacement.
func UnifiedDiff(aName, bName, a, b string, context int) string {
	if a == b {
		return ""
	}
	al, bl := Lines(a), Lines(b)
	pre := 0
	for pre < len(al) && pre < len(bl) && al[pre] == bl[pre] {
		pre++
	}
	suf := 0
	for suf < len(al)-pre && suf < len(bl)-pre && al[len(al)-1-suf] == bl[len(bl)-1-suf] {
		suf++
	}
	am, bm := al[pre:len(al)-suf], bl[pre:len(bl)-suf]

	type op struct {
		kind byte // ' ', '-', '+'
		text string
		ai   int // 0-based line in a (for ' ' and '-')
		bi   int
	}
	var ops []op
	for i := 0; i < pre; i++ {
		ops = append(ops, op{' ', al[i], i, i})
	}
	if (len(am)+1)*(len(bm)+1) > maxDiffCells {
		for i, l := range am {
			ops = append(ops, op{'-', l, pre + i, pre})
		}
		for j, l := range bm {
			ops = append(ops, op{'+', l, pre + len(am), pre + j})
		}
	} else {
		n, m := len(am), len(bm)
		lcs := make([]int32, (n+1)*(m+1))
		at := func(i, j int) int32 { return lcs[i*(m+1)+j] }
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if am[i] == bm[j] {
					lcs[i*(m+1)+j] = at(i+1, j+1) + 1
				} else {
					lcs[i*(m+1)+j] = max(at(i+1, j), at(i, j+1))
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && am[i] == bm[j]:
				ops = append(ops, op{' ', am[i], pre + i, pre + j})
				i++
				j++
			case i < n && (j == m || at(i+1, j) >= at(i, j+1)):
				ops = append(ops, op{'-', am[i], pre + i, pre + j})
				i++
			default:
				ops = append(ops, op{'+', bm[j], pre + i, pre + j})
				j++
			}
		}
	}
	for k := 0; k < suf; k++ {
		ai, bi := len(al)-suf+k, len(bl)-suf+k
		ops = append(ops, op{' ', al[ai], ai, bi})
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", aName, bName)
	// Group changes into hunks with context.
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		start := max(k-context, 0)
		end := k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
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
		aStart, bStart, aN, bN := ops[start].ai, ops[start].bi, 0, 0
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				aN++
			}
			if o.kind != '-' {
				bN++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart+1, aN, bStart+1, bN)
		for _, o := range ops[start:end] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		k = end
	}
	return out.String()
}
