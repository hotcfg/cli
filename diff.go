package cli

import (
	"fmt"
	"strings"
)

const (
	ansiReset = "\x1b[0m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiCyan  = "\x1b[36m"
	ansiBold  = "\x1b[1m"
	ansiDim   = "\x1b[2m"
)

type editKind byte

const (
	editEqual  editKind = ' '
	editDelete editKind = '-'
	editInsert editKind = '+'
)

type edit struct {
	kind editKind
	line string
}

// maxDiffD bounds the Myers search (the number of changed lines it will
// look for). Past it the diff degrades to "delete all, insert all", which
// is still correct, just not minimal.
const maxDiffD = 10000

// diffLines returns a minimal edit script turning a into b (Myers'
// O(ND) algorithm).
func diffLines(a, b []string) []edit {
	// Common prefix and suffix are cheap to strip and usually most of a
	// config file.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var out []edit
	for _, l := range a[:pre] {
		out = append(out, edit{editEqual, l})
	}
	out = append(out, myers(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for _, l := range a[len(a)-suf:] {
		out = append(out, edit{editEqual, l})
	}
	return out
}

func myers(a, b []string) []edit {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	max := n + m
	if max > maxDiffD {
		max = maxDiffD
	}
	// v[k+off] is the furthest x reached on diagonal k. trace[d] is a copy
	// of v's live range before step d, for backtracking.
	off := max + 1
	v := make([]int, 2*off+1)
	var trace [][]int
	found := false
	for d := 0; d <= max && !found; d++ {
		snap := make([]int, 2*d+3)
		copy(snap, v[off-d-1:off+d+2])
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1] // down: insertion
			} else {
				x = v[off+k-1] + 1 // right: deletion
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	if !found {
		out := make([]edit, 0, n+m)
		for _, l := range a {
			out = append(out, edit{editDelete, l})
		}
		for _, l := range b {
			out = append(out, edit{editInsert, l})
		}
		return out
	}

	// Walk back from (n, m) through the snapshots.
	var rev []edit
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		snap := trace[d] // index k+d+1 holds diagonal k
		at := func(k int) int { return snap[k+d+1] }
		k := x - y
		var prevK int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			rev = append(rev, edit{editEqual, a[x-1]})
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				rev = append(rev, edit{editInsert, b[y-1]})
			} else {
				rev = append(rev, edit{editDelete, a[x-1]})
			}
			x, y = prevX, prevY
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// unifiedDiff renders a unified diff with ctx lines of context, or "" if a
// and b are equal. With color, deletions are red and insertions green.
func unifiedDiff(nameA, nameB string, a, b []string, ctx int, color bool) string {
	edits := diffLines(a, b)
	// aBefore[i], bBefore[i]: lines of a and b consumed by edits[:i].
	aBefore := make([]int, len(edits)+1)
	bBefore := make([]int, len(edits)+1)
	var changes []int
	for i, e := range edits {
		aBefore[i+1], bBefore[i+1] = aBefore[i], bBefore[i]
		if e.kind != editInsert {
			aBefore[i+1]++
		}
		if e.kind != editDelete {
			bBefore[i+1]++
		}
		if e.kind != editEqual {
			changes = append(changes, i)
		}
	}
	if len(changes) == 0 {
		return ""
	}

	paint := func(code, s string) string {
		if !color {
			return s
		}
		return code + s + ansiReset
	}
	var sb strings.Builder
	sb.WriteString(paint(ansiBold, "--- "+nameA) + "\n")
	sb.WriteString(paint(ansiBold, "+++ "+nameB) + "\n")

	for i := 0; i < len(changes); {
		// Extend the hunk while the next change is within 2*ctx lines.
		j := i
		for j+1 < len(changes) && changes[j+1]-changes[j] <= 2*ctx+1 {
			j++
		}
		start := max(0, changes[i]-ctx)
		end := min(len(edits), changes[j]+ctx+1)
		aStart, aCount := aBefore[start]+1, aBefore[end]-aBefore[start]
		bStart, bCount := bBefore[start]+1, bBefore[end]-bBefore[start]
		if aCount == 0 {
			aStart-- // an empty range names the line before it
		}
		if bCount == 0 {
			bStart--
		}
		sb.WriteString(paint(ansiCyan, fmt.Sprintf("@@ -%s +%s @@", hunkRange(aStart, aCount), hunkRange(bStart, bCount))) + "\n")
		for _, e := range edits[start:end] {
			line := string(e.kind) + e.line
			switch e.kind {
			case editDelete:
				line = paint(ansiRed, line)
			case editInsert:
				line = paint(ansiGreen, line)
			}
			sb.WriteString(line + "\n")
		}
		i = j + 1
	}
	return sb.String()
}

func hunkRange(start, count int) string {
	if count == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
