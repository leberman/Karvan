// Package merge holds the merge rules of docs/sync-engine.md section 8.
package merge

import "bytes"

// maxCells bounds the LCS table; bigger inputs are reported as conflicts
// rather than risking time and memory (both versions are kept then).
const maxCells = 16_000_000

// Text3 merges ours and theirs line by line against base (diff3). ok is false
// when the same region changed differently on both sides, or the files are too
// large to compare; then the caller keeps both versions.
func Text3(base, ours, theirs []byte) (merged []byte, ok bool) {
	switch {
	case bytes.Equal(ours, theirs), bytes.Equal(base, theirs):
		return ours, true
	case bytes.Equal(base, ours):
		return theirs, true
	}
	b, o, t := splitLines(base), splitLines(ours), splitLines(theirs)
	mo, ok1 := lcsMap(b, o)
	mt, ok2 := lcsMap(b, t)
	if !ok1 || !ok2 {
		return nil, false
	}
	var out [][]byte
	ib, io, it := 0, 0, 0
	for ib < len(b) || io < len(o) || it < len(t) {
		if ib < len(b) && mo[ib] == io && mt[ib] == it {
			out = append(out, b[ib])
			ib, io, it = ib+1, io+1, it+1
			continue
		}
		// Next base line present (and in order) on both sides.
		j := ib
		for j < len(b) && (mo[j] < io || mt[j] < it) {
			j++
		}
		oj, tj := len(o), len(t)
		if j < len(b) {
			oj, tj = mo[j], mt[j]
		}
		bc, oc, tc := b[ib:j], o[io:oj], t[it:tj]
		switch {
		case equalLines(oc, bc):
			out = append(out, tc...)
		case equalLines(tc, bc), equalLines(oc, tc):
			out = append(out, oc...)
		default:
			return nil, false
		}
		ib, io, it = j, oj, tj
	}
	return bytes.Join(out, nil), true
}

// splitLines splits after each "\n", keeping it.
func splitLines(s []byte) [][]byte {
	var out [][]byte
	for len(s) > 0 {
		i := bytes.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

func equalLines(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// lcsMap returns, for each line of a, its index in b in a longest common
// subsequence, or -1.
func lcsMap(a, b [][]byte) ([]int, bool) {
	m := make([]int, len(a))
	for i := range m {
		m[i] = -1
	}
	// Trim common prefix and suffix first; most edits are small.
	p := 0
	for p < len(a) && p < len(b) && bytes.Equal(a[p], b[p]) {
		m[p] = p
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && bytes.Equal(a[len(a)-1-s], b[len(b)-1-s]) {
		m[len(a)-1-s] = len(b) - 1 - s
		s++
	}
	ai, bi := a[p:len(a)-s], b[p:len(b)-s]
	n, k := len(ai), len(bi)
	if n == 0 || k == 0 {
		return m, true
	}
	if n*k > maxCells {
		return nil, false
	}
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, k+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := k - 1; j >= 0; j-- {
			if bytes.Equal(ai[i], bi[j]) {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	for i, j := 0, 0; i < n && j < k; {
		switch {
		case bytes.Equal(ai[i], bi[j]):
			m[p+i] = p + j
			i, j = i+1, j+1
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	return m, true
}
