package history

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxBoundaryRawBytes        = 1 << 20
	MaxBoundaryNormalizedBytes = 256 << 10
	MaxBoundaryTokens          = 16_384
	MaxBoundaryCandidates      = 64
	MaxBoundaryComparisons     = 65_536
	MinBoundaryWords           = 12
	MinBoundaryRows            = 2
)

type boundaryToken struct {
	text string
	row  int
}
type boundaryView struct {
	rows     []string
	tokens   []boundaryToken
	firstRow int
	guard    []boundaryToken
}
type boundaryWork struct {
	rawBytes, normalizedBytes, tokens, candidates, comparisons int
	normalizedRows                                             int
}
type boundaryResult struct {
	cut, rows, seam, words, substantiveRows int
	refused                                 bool
	work                                    boundaryWork
}

func (work *boundaryWork) row(raw string) (string, bool) {
	if len(raw) > MaxBoundaryRawBytes-work.rawBytes {
		return "", false
	}
	work.rawBytes += len(raw)
	length, trailing := 0, 0
	boundaryPlainSpans(raw, func(text string) {
		for len(text) > 0 {
			char, size := utf8.DecodeRuneInString(text)
			text = text[size:]
			if char == '\r' {
				continue
			}
			if char == ' ' {
				char, size = ' ', 1
			}
			length += size
			if char == ' ' || char == '\t' || char == '\n' {
				trailing += size
			} else {
				trailing = 0
			}
		}
	})
	length -= trailing
	if length > MaxBoundaryNormalizedBytes-work.normalizedBytes {
		return "", false
	}
	if length == 0 {
		work.normalizedRows++
		return "", true
	}
	if len(raw) > MaxBoundaryRawBytes-work.rawBytes {
		return "", false
	}
	work.rawBytes += len(raw)
	var view strings.Builder
	view.Grow(length)
	boundaryPlainSpans(raw, func(text string) {
		for len(text) > 0 && view.Len() < length {
			char, size := utf8.DecodeRuneInString(text)
			part := text[:size]
			text = text[size:]
			if char == '\r' {
				continue
			}
			if char == ' ' {
				part = " "
			}
			view.WriteString(part)
		}
	})
	work.normalizedBytes += length
	work.normalizedRows++
	return view.String(), true
}

func boundaryPlainSpans(raw string, visit func(string)) {
	position := 0
	for position < len(raw) {
		span := ansiRe.FindStringIndex(raw[position:])
		if span == nil {
			break
		}
		visit(raw[position : position+span[0]])
		position += span[1]
	}
	visit(raw[position:])
}

func (work *boundaryWork) equal(a, b string) (bool, bool) {
	if work.comparisons == MaxBoundaryComparisons {
		return false, false
	}
	work.comparisons++
	return a == b, true
}
func (work *boundaryWork) addTokens(row string, index int) ([]boundaryToken, bool) {
	count, inWord := 0, false
	for _, char := range row {
		if unicode.IsSpace(char) {
			inWord = false
			continue
		}
		if !inWord {
			count++
			inWord = true
		}
		if count > MaxBoundaryTokens-work.tokens {
			return nil, false
		}
	}
	fields := strings.Fields(row)
	work.tokens += count
	tokens := make([]boundaryToken, len(fields))
	for i, field := range fields {
		tokens[i] = boundaryToken{field, index}
	}
	return tokens, true
}
func boundaryIncoming(rows []string, work *boundaryWork) (boundaryView, bool) {
	view := boundaryView{}
	for i, raw := range rows {
		row, ok := work.row(raw)
		if !ok {
			return boundaryView{}, false
		}
		view.rows = append(view.rows, row)
		tokens, ok := work.addTokens(row, i)
		if !ok {
			return boundaryView{}, false
		}
		view.tokens = append(view.tokens, tokens...)
	}
	return view, true
}
func boundaryArchive(rows []string, floor int, limit int, work *boundaryWork) (boundaryView, bool) {
	view := boundaryView{firstRow: len(rows)}
	length := 0
	var reverse [][]boundaryToken
	for i := len(rows) - 1; i >= floor; i-- {
		row, ok := work.row(rows[i])
		if !ok {
			return boundaryView{}, false
		}
		if length >= limit && length > 0 {
			view.guard, ok = work.addTokens(row, i)
			if !ok {
				return boundaryView{}, false
			}
			break
		}
		length += len(row)
		tokens, ok := work.addTokens(row, i)
		if !ok {
			return boundaryView{}, false
		}
		view.rows = append(view.rows, row)
		reverse = append(reverse, tokens)
		view.firstRow = i
	}
	slices.Reverse(view.rows)
	for i := len(reverse) - 1; i >= 0; i-- {
		view.tokens = append(view.tokens, reverse[i]...)
	}
	return view, true
}
func boundaryUnsafeRow(row string) bool {
	return strings.ContainsAny(row, "`{}[]|/\\") || strings.ContainsFunc(row, func(r rune) bool { return unicode.IsMark(r) })
}
func boundaryOccurrences(haystack, needle []boundaryToken, work *boundaryWork) (int, bool) {
	count := 0
	for start := 0; start+len(needle) <= len(haystack); start++ {
		match := true
		for j, token := range needle {
			equal, ok := work.equal(haystack[start+j].text, token.text)
			if !ok {
				return 0, false
			}
			if !equal {
				match = false
				break
			}
		}
		if match {
			count++
		}
	}
	return count, true
}

func matchBoundarySuffix(history []string, floor int, incoming []string) boundaryResult {
	result := boundaryResult{cut: len(history), refused: true}
	work := &result.work
	next, ok := boundaryIncoming(incoming, work)
	if !ok || len(next.tokens) == 0 {
		return result
	}
	limit := 0
	for _, row := range next.rows {
		limit += len(row)
	}
	old, ok := boundaryArchive(history, floor, limit, work)
	if !ok || len(old.tokens) == 0 || old.tokens[len(old.tokens)-1].row != len(history)-1 {
		return result
	}
	last := old.tokens[len(old.tokens)-1].text
	var accepted *boundaryResult
	for end, token := range next.tokens {
		equal, ok := work.equal(token.text, last)
		if !ok {
			return result
		}
		if !equal {
			continue
		}
		if work.candidates == MaxBoundaryCandidates {
			return result
		}
		work.candidates++
		n := 0
		for n < len(old.tokens) && n <= end {
			equal, ok := work.equal(old.tokens[len(old.tokens)-1-n].text, next.tokens[end-n].text)
			if !ok {
				return result
			}
			if !equal {
				break
			}
			n++
		}
		if n < MinBoundaryWords {
			continue
		}
		start := len(old.tokens) - n
		newStart := end - n + 1
		first := old.tokens[start].row
		partial := start > 0 && old.tokens[start-1].row == first
		if partial && newStart != 0 {
			continue
		}
		cut := first
		if partial {
			cut++
		}
		fullWords, subRows, proseRows := 0, 0, 0
		prompt := false
		unsafe := false
		for i := first; i < len(history); i++ {
			row := old.rows[i-old.firstRow]
			if row == "" {
				prompt = false
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(row), "❯") {
				prompt = true
			}
			if boundaryUnsafeRow(row) {
				unsafe = true
				break
			}
			if i < cut {
				continue
			}
			if physicalSubstantive(row) {
				subRows++
				if !prompt {
					proseRows++
				}
			}
			fullWords += len(strings.Fields(row))
		}
		for i := next.tokens[newStart].row; !unsafe && i <= token.row; i++ {
			unsafe = boundaryUnsafeRow(next.rows[i])
		}
		if unsafe || fullWords < MinBoundaryWords || subRows < MinBoundaryRows || proseRows < MinBoundaryRows {
			continue
		}
		needle := old.tokens[start:]
		if len(old.guard) > 0 && len(old.guard) <= len(needle) {
			guardEqual := true
			for j, token := range old.guard {
				equal, ok := work.equal(token.text, needle[len(needle)-len(old.guard)+j].text)
				if !ok {
					return result
				}
				if !equal {
					guardEqual = false
					break
				}
			}
			if guardEqual {
				return result
			}
		}
		oldCount, ok := boundaryOccurrences(old.tokens, needle, work)
		if !ok {
			return result
		}
		newCount, ok := boundaryOccurrences(next.tokens, needle, work)
		if !ok {
			return result
		}
		if oldCount != 1 || newCount != 1 {
			continue
		}
		if old.firstRow > floor && start > 0 {
			competing := 0
			for competing < start && competing < len(needle) {
				equal, ok := work.equal(old.tokens[start-1-competing].text, needle[len(needle)-1-competing].text)
				if !ok {
					return result
				}
				if !equal {
					break
				}
				competing++
			}
			if competing > 0 && old.tokens[start-competing].row < first {
				return result
			}
		}
		if accepted != nil {
			return result
		}
		candidate := boundaryResult{cut: cut, rows: len(history) - cut, words: fullWords, substantiveRows: subRows}
		if partial {
			candidate.seam = 1
		}
		accepted = &candidate
	}
	if accepted == nil {
		return result
	}
	accepted.work = *work
	return *accepted
}

func reconcileBoundary(state *PaneState, body []string, supported bool) boundaryResult {
	result := boundaryResult{cut: len(state.History), refused: true}
	if supported {
		result = matchBoundarySuffix(state.History, state.SegmentStart, body)
	}
	if result.rows == 0 {
		return result
	}
	state.History = state.History[:result.cut]
	state.LiveStart = result.cut
	state.SegmentStart = min(state.SegmentStart, result.cut)
	clampHeldPins(state)
	state.archiveMutated()
	return result
}
