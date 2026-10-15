package gitx

import "bytes"

// LineDiff is an in-process Myers diff of old and new, split on newlines.
// Old and New are the changed ranges in the same shape as parseUnifiedZero,
// one pair per hunk, paths left empty. A pure insertion is a zero-width old
// range (End == Start-1) anchored at the base line git prints for a `-L,0`
// hunk; a pure deletion is a zero-width new range. The newline is part of
// each line, so a missing trailing newline is a change to that line.
func LineDiff(old, new []byte) FileHunks {
	oldR, newR := rangesFromOps(myersOps(splitLines(old), splitLines(new)))
	return FileHunks{Old: oldR, New: newR}
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	var lines []string
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			lines = append(lines, string(b))
			break
		}
		lines = append(lines, string(b[:i+1]))
		b = b[i+1:]
	}
	return lines
}

type op uint8

const (
	opMove op = iota
	opInsert
	opDelete
)

// myersOps is Myers' shortest edit script. Equal lines are opMove.
// The search is the formulation in Myers 1986: diagonal k = x-y, extended
// by snakes of equal lines. See also the walk-back used by most ports of it.
func myersOps(src, dst []string) []op {
	n, m := len(src), len(dst)
	max := n + m
	var trace []map[int]int
	var x, y int

	for d := 0; d <= max; d++ {
		v := make(map[int]int, d+2)
		trace = append(trace, v)
		if d == 0 {
			t := 0
			for t < n && t < m && src[t] == dst[t] {
				t++
			}
			v[0] = t
			if t == n && t == m {
				break
			}
			continue
		}
		last := trace[d-1]
		done := false
		for k := -d; k <= d; k += 2 {
			if k == -d || (k != d && last[k-1] < last[k+1]) {
				x = last[k+1]
			} else {
				x = last[k-1] + 1
			}
			y = x - k
			for x < n && y < m && src[x] == dst[y] {
				x++
				y++
			}
			v[k] = x
			if x == n && y == m {
				done = true
				break
			}
		}
		if done {
			break
		}
	}

	var script []op
	x, y = n, m
	for d := len(trace) - 1; d > 0; d-- {
		k := x - y
		last := trace[d-1]
		var prevK int
		if k == -d || (k != d && last[k-1] < last[k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := last[prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			script = append(script, opMove)
			x--
			y--
		}
		if x == prevX {
			script = append(script, opInsert)
		} else {
			script = append(script, opDelete)
		}
		x, y = prevX, prevY
	}
	if len(trace) > 0 {
		for i := 0; i < trace[0][0]; i++ {
			script = append(script, opMove)
		}
	}
	for i, j := 0, len(script)-1; i < j; i, j = i+1, j-1 {
		script[i], script[j] = script[j], script[i]
	}
	return script
}

func rangesFromOps(ops []op) (oldR, newR []LineRange) {
	oldLine, newLine := 1, 1
	for i := 0; i < len(ops); {
		if ops[i] == opMove {
			oldLine++
			newLine++
			i++
			continue
		}
		oStart, nStart := oldLine, newLine
		oCount, nCount := 0, 0
		for i < len(ops) && ops[i] != opMove {
			switch ops[i] {
			case opDelete:
				oCount++
				oldLine++
			case opInsert:
				nCount++
				newLine++
			}
			i++
		}
		oldR = append(oldR, lineSpan(oStart, oCount))
		newR = append(newR, lineSpan(nStart, nCount))
	}
	return oldR, newR
}

// lineSpan is an inclusive 1-based range. A zero count is the zero-width
// anchor git prints: the line before start, with End one less than Start.
func lineSpan(start, count int) LineRange {
	if count == 0 {
		return LineRange{Start: start - 1, End: start - 2}
	}
	return LineRange{Start: start, End: start + count - 1}
}
