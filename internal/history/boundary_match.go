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
type boundaryContext uint8

const (
	boundaryContextUnknown boundaryContext = iota
	boundaryContextProse
	boundaryContextPrompt
)

type boundaryView struct {
	rows     []string
	tokens   []boundaryToken
	contexts []boundaryContext
	firstRow int
}
type boundaryWork struct {
	rawBytes, normalizedBytes, tokens, candidates, comparisons int
	normalizedRows                                             int
}
type boundaryRefusalReason string

const (
	boundaryShort              boundaryRefusalReason = "short"
	boundaryUnsafeStructure    boundaryRefusalReason = "unsafe-structure"
	boundaryPromptContext      boundaryRefusalReason = "prompt-context"
	boundaryAmbiguousCandidate boundaryRefusalReason = "ambiguous-candidate"
	boundaryPartialRun         boundaryRefusalReason = "partial-run"
	boundaryIncomingPrompt     boundaryRefusalReason = "incoming-prompt"
	boundaryBudget             boundaryRefusalReason = "budget"
	boundaryNoCandidate        boundaryRefusalReason = "no-candidate"
)

type boundaryRun struct {
	side                                             string
	firstRow, lastRow, needleFirstRow, needleLastRow int
	words                                            int
}

type boundaryResult struct {
	cut, rows, seam, words, substantiveRows int
	refused                                 bool
	reason                                  boundaryRefusalReason
	competing                               boundaryRun
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
func boundaryRowContexts(rows []string) []boundaryContext {
	contexts := make([]boundaryContext, len(rows))
	state := boundaryContextUnknown
	for i, row := range rows {
		if row == "" {
			state = boundaryContextProse
		} else if strings.HasPrefix(strings.TrimSpace(row), "❯") {
			state = boundaryContextPrompt
		}
		contexts[i] = state
	}
	return contexts
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
	view.contexts = boundaryRowContexts(view.rows)
	return view, true
}
func boundaryArchive(rows []string, floor int, limit int, work *boundaryWork) (boundaryView, bool) {
	view := boundaryView{firstRow: len(rows)}
	var reverse [][]boundaryToken
	length, windows := 0, 0
	// Inspect two incoming-length windows, then preceding layout through the nearest
	// substantive row. The segment floor and work budgets also bound discovery.
	for i := len(rows) - 1; i >= floor; i-- {
		context := windows == 2
		row, ok := work.row(rows[i])
		if !ok {
			return boundaryView{}, false
		}
		tokens, ok := work.addTokens(row, i)
		if !ok {
			return boundaryView{}, false
		}
		view.rows = append(view.rows, row)
		reverse = append(reverse, tokens)
		view.firstRow = i
		length += len(row)
		if !context && length >= limit {
			windows++
			length = 0
		}
		if context && physicalSubstantive(row) {
			break
		}
	}
	slices.Reverse(view.rows)
	for i := len(reverse) - 1; i >= 0; i-- {
		view.tokens = append(view.tokens, reverse[i]...)
	}
	view.contexts = boundaryRowContexts(view.rows)
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

func boundaryCompetingRun(haystack, needle []boundaryToken, view boundaryView, work *boundaryWork) (boundaryRun, bool) {
	prose := make([]bool, len(view.rows))
	prompt := false
	for i, row := range view.rows {
		if row == "" {
			prompt = false
		} else if strings.HasPrefix(strings.TrimSpace(row), "❯") {
			prompt = true
		}
		prose[i] = !prompt && physicalSubstantive(row)
	}
	starts := make(map[string][]int)
	for start := 0; start+MinBoundaryWords <= len(needle); start++ {
		starts[needle[start].text] = append(starts[needle[start].text], start)
	}
	for start := 0; start+MinBoundaryWords <= len(haystack); start++ {
		for _, candidate := range starts[haystack[start].text] {
			proseRows, previousRow := 0, -1
			for j := 0; start+j < len(haystack) && candidate+j < len(needle); j++ {
				equal, ok := work.equal(haystack[start+j].text, needle[candidate+j].text)
				if !ok {
					return boundaryRun{}, false
				}
				if !equal {
					break
				}
				row := haystack[start+j].row
				if row != previousRow && prose[row-view.firstRow] {
					proseRows++
				}
				previousRow = row
				if j+1 >= MinBoundaryWords && proseRows >= MinBoundaryRows {
					return boundaryRun{
						firstRow: haystack[start].row, lastRow: row,
						needleFirstRow: needle[candidate].row, needleLastRow: needle[candidate+j].row,
						words: j + 1,
					}, true
				}
			}
		}
	}
	return boundaryRun{}, true
}

func boundaryPrecedingSubstantive(view boundaryView, before int) int {
	for row := before - 1; row >= view.firstRow; row-- {
		if physicalSubstantive(view.rows[row-view.firstRow]) {
			return row
		}
	}
	return -1
}

func boundaryPromptBlock(view boundaryView, before int) (first, last int, found bool) {
	for row := before - 1; row >= view.firstRow; row-- {
		if !strings.HasPrefix(strings.TrimSpace(view.rows[row-view.firstRow]), "❯") {
			continue
		}
		last = row
		for last+1 < before && last+1 < view.firstRow+len(view.rows) && view.rows[last+1-view.firstRow] != "" {
			last++
		}
		return row, last, true
	}
	return 0, 0, false
}

func boundaryArchivePrompt(history []string, view *boundaryView, before int, work *boundaryWork) (first, last int, found, ok bool) {
	var earlierRows []string
	var earlierTokens [][]boundaryToken
	for row := before - 1; row >= 0; row-- {
		var text string
		if row >= view.firstRow {
			text = view.rows[row-view.firstRow]
		} else {
			var valid bool
			text, valid = work.row(history[row])
			if !valid {
				return 0, 0, false, false
			}
			tokens, valid := work.addTokens(text, row)
			if !valid {
				return 0, 0, false, false
			}
			earlierRows = append(earlierRows, text)
			earlierTokens = append(earlierTokens, tokens)
		}
		if strings.HasPrefix(strings.TrimSpace(text), "❯") {
			first, found = row, true
			break
		}
	}
	if len(earlierRows) > 0 {
		slices.Reverse(earlierRows)
		view.rows = append(earlierRows, view.rows...)
		var tokens []boundaryToken
		for i := len(earlierTokens) - 1; i >= 0; i-- {
			tokens = append(tokens, earlierTokens[i]...)
		}
		view.tokens = append(tokens, view.tokens...)
		view.firstRow -= len(earlierRows)
	}
	if !found {
		return 0, 0, false, true
	}
	last = first
	for last+1 < before && view.rows[last+1-view.firstRow] != "" {
		last++
	}
	return first, last, true, true
}

func boundaryBlockTokens(view boundaryView, first, last int) []boundaryToken {
	start := 0
	for start < len(view.tokens) && view.tokens[start].row < first {
		start++
	}
	end := start
	for end < len(view.tokens) && view.tokens[end].row <= last {
		end++
	}
	return view.tokens[start:end]
}

func boundaryRowStartMarker(view boundaryView, index int) bool {
	row := view.tokens[index].row
	return (index == 0 || view.tokens[index-1].row != row) &&
		strings.HasPrefix(strings.TrimSpace(view.rows[row-view.firstRow]), "❯")
}

func boundaryBlankGap(view boundaryView, previous, current int) bool {
	for row := previous + 1; row < current; row++ {
		if view.rows[row-view.firstRow] == "" {
			return true
		}
	}
	return false
}

func matchBoundarySuffix(history []string, floor int, incoming []string) boundaryResult {
	result := boundaryResult{cut: len(history), refused: true, reason: boundaryNoCandidate}
	work := &result.work
	next, ok := boundaryIncoming(incoming, work)
	if !ok {
		result.reason = boundaryBudget
		return result
	}
	if len(next.tokens) == 0 {
		return result
	}
	limit := 0
	for _, row := range next.rows {
		limit += len(row)
	}
	old, ok := boundaryArchive(history, floor, limit, work)
	if !ok {
		result.reason = boundaryBudget
		return result
	}
	if len(old.tokens) == 0 || old.tokens[len(old.tokens)-1].row != len(history)-1 {
		return result
	}
	matchFloor, length := len(history), 0
	for i := len(old.rows) - 1; i >= 0 && (length < limit || length == 0); i-- {
		length += len(old.rows[i])
		matchFloor = old.firstRow + i
	}
	last := old.tokens[len(old.tokens)-1].text
	promptView := old
	var accepted *boundaryResult
	for end, token := range next.tokens {
		equal, ok := work.equal(token.text, last)
		if !ok {
			result.reason = boundaryBudget
			return result
		}
		if !equal {
			continue
		}
		if work.candidates == MaxBoundaryCandidates {
			result.reason = boundaryBudget
			return result
		}
		work.candidates++
		n := 0
		for n < len(old.tokens) && n <= end && old.tokens[len(old.tokens)-1-n].row >= matchFloor {
			equal, ok := work.equal(old.tokens[len(old.tokens)-1-n].text, next.tokens[end-n].text)
			if !ok {
				result.reason = boundaryBudget
				return result
			}
			if !equal {
				break
			}
			n++
		}
		if n < MinBoundaryWords {
			result.reason = boundaryShort
			continue
		}
		start := len(old.tokens) - n
		newStart := end - n + 1
		first := old.tokens[start].row
		partial := start > 0 && old.tokens[start-1].row == first
		if partial && newStart != 0 {
			result.reason = boundaryUnsafeStructure
			continue
		}
		cut := first
		if partial {
			cut++
		}
		fullWords, subRows, proseRows := 0, 0, 0
		prompt := false
		contextKnown := old.firstRow == floor
		for i := first - 1; i >= old.firstRow; i-- {
			row := old.rows[i-old.firstRow]
			if row == "" {
				contextKnown = true
				break
			}
			if strings.HasPrefix(strings.TrimSpace(row), "❯") {
				prompt = true
			}
		}
		if !contextKnown {
			result.reason = boundaryPromptContext
			continue
		}
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
		if unsafe {
			result.reason = boundaryUnsafeStructure
			continue
		}
		if fullWords < MinBoundaryWords || subRows < MinBoundaryRows {
			result.reason = boundaryShort
			continue
		}
		if proseRows < MinBoundaryRows {
			result.reason = boundaryPromptContext
			continue
		}
		incomingRow := next.tokens[newStart].row
		for row := incomingRow; row >= next.firstRow; row-- {
			text := next.rows[row-next.firstRow]
			if text == "" {
				break
			}
			if !strings.HasPrefix(strings.TrimSpace(text), "❯") {
				continue
			}
			if row != incomingRow || newStart > 0 && next.tokens[newStart-1].row == incomingRow {
				result.reason = boundaryIncomingPrompt
				return result
			}
			break
		}
		for k := range n {
			if boundaryRowStartMarker(next, newStart+k) != boundaryRowStartMarker(old, start+k) {
				result.reason = boundaryIncomingPrompt
				return result
			}
		}
		for k := range n {
			archiveContext := old.contexts[old.tokens[start+k].row-old.firstRow]
			incomingContext := next.contexts[next.tokens[newStart+k].row-next.firstRow]
			if archiveContext != boundaryContextUnknown && incomingContext != boundaryContextUnknown && archiveContext != incomingContext {
				result.reason = boundaryIncomingPrompt
				return result
			}
		}
		for k := 1; k < n; k++ {
			archiveGap := boundaryBlankGap(old, old.tokens[start+k-1].row, old.tokens[start+k].row)
			incomingGap := boundaryBlankGap(next, next.tokens[newStart+k-1].row, next.tokens[newStart+k].row)
			if archiveGap != incomingGap {
				result.reason = boundaryUnsafeStructure
				return result
			}
		}
		incomingFirst, incomingLast, incomingPrompt := boundaryPromptBlock(next, incomingRow)
		if incomingPrompt {
			archiveFirst, archiveLast, archivePrompt, ok := boundaryArchivePrompt(history, &promptView, first, work)
			if !ok {
				result.reason = boundaryBudget
				return result
			}
			if !archivePrompt {
				result.reason = boundaryIncomingPrompt
				return result
			}
			incomingTokens := boundaryBlockTokens(next, incomingFirst, incomingLast)
			archiveTokens := boundaryBlockTokens(promptView, archiveFirst, archiveLast)
			if len(incomingTokens) != len(archiveTokens) {
				result.reason = boundaryIncomingPrompt
				return result
			}
			for i, token := range incomingTokens {
				equal, ok := work.equal(token.text, archiveTokens[i].text)
				if !ok {
					result.reason = boundaryBudget
					return result
				}
				if !equal {
					result.reason = boundaryIncomingPrompt
					return result
				}
			}
		}
		needle := old.tokens[start:]
		oldCount, ok := boundaryOccurrences(old.tokens, needle, work)
		if !ok {
			result.reason = boundaryBudget
			return result
		}
		newCount, ok := boundaryOccurrences(next.tokens, needle, work)
		if !ok {
			result.reason = boundaryBudget
			return result
		}
		if oldCount != 1 || newCount != 1 {
			result.reason = boundaryAmbiguousCandidate
			return result
		}
		for side, outside := range [][]boundaryToken{old.tokens[:start], next.tokens[:newStart], next.tokens[end+1:]} {
			view := next
			if side == 0 {
				view = old
			}
			competing, ok := boundaryCompetingRun(outside, needle, view, work)
			if !ok {
				result.reason = boundaryBudget
				return result
			}
			if competing.words > 0 {
				competing.side = "incoming"
				if side == 0 {
					competing.side = "archive"
				}
				result.reason, result.competing = boundaryPartialRun, competing
				return result
			}
		}
		if accepted != nil {
			result.reason = boundaryAmbiguousCandidate
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
	result := boundaryResult{cut: len(state.History), refused: true, reason: boundaryUnsafeStructure}
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
