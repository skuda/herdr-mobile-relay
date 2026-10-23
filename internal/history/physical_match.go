package history

import (
	"slices"
	"strings"
	"unicode"
)

const MaxPhysicalMatchPairs = 65_536

type physicalPair struct{ Old, Next int }
type physicalConsensusResult struct {
	agreed, feasible []physicalPair
	pairs            int
	budget           bool
	path             string
}

func physicalComparisonRow(row string) string {
	return strings.TrimRight(strings.ReplaceAll(NormalizeLine(row), " ", " "), " \t\n")
}
func physicalSubstantive(row string) bool {
	return strings.ContainsFunc(row, func(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) })
}
func comparisonBody(body []string) []string {
	rows := make([]string, len(body))
	for i, row := range body {
		rows[i] = physicalComparisonRow(row)
	}
	for len(rows) > 0 && rows[len(rows)-1] == "" {
		rows = rows[:len(rows)-1]
	}
	return rows
}

// physicalConsensus consumes physical comparison rows, not raw terminal rows.
func physicalConsensus(old, next []string, linear bool) physicalConsensusResult {
	oldIndex, newIndex := make(map[string][]int), make(map[string][]int)
	for i, row := range old {
		if physicalSubstantive(row) {
			oldIndex[row] = append(oldIndex[row], i)
		}
	}
	for i, row := range next {
		if physicalSubstantive(row) {
			newIndex[row] = append(newIndex[row], i)
		}
	}
	result := physicalConsensusResult{path: "sparse"}
	for row, indices := range oldIndex {
		count := len(newIndex[row])
		if count == 0 {
			continue
		}
		if len(indices) > (MaxPhysicalMatchPairs-result.pairs)/count {
			result.pairs = MaxPhysicalMatchPairs + 1
			break
		}
		result.pairs += len(indices) * count
	}
	if linear {
		// A full shared-substantive sequence fixes its occurrence ranks: an
		// optimum must consume that sequence on both sides.
		var a, b []physicalPair
		for i, row := range old {
			if len(newIndex[row]) > 0 {
				a = append(a, physicalPair{Old: i})
			}
		}
		for j, row := range next {
			if len(oldIndex[row]) > 0 {
				b = append(b, physicalPair{Next: j})
			}
		}
		equal := len(a) == len(b)
		for k := 0; equal && k < len(a); k++ {
			equal = old[a[k].Old] == next[b[k].Next]
		}
		if equal {
			for k := range a {
				a[k].Next = b[k].Next
			}
			result.agreed = a
			result.feasible = slices.Clone(a)
			result.path = "linear"
			return result
		}
	}
	if result.pairs > MaxPhysicalMatchPairs {
		result.budget = true
		return result
	}
	pairs := make([]physicalPair, 0, result.pairs)
	for i, row := range old {
		for _, j := range newIndex[row] {
			pairs = append(pairs, physicalPair{i, j})
		}
	}
	forward, backward := make([]int, len(pairs)), make([]int, len(pairs))
	tree := make([]int, len(next)+1)
	query := func(index int) int {
		best := 0
		for index > 0 {
			best = max(best, tree[index])
			index -= index & -index
		}
		return best
	}
	update := func(index, value int) {
		for index < len(tree) {
			tree[index] = max(tree[index], value)
			index += index & -index
		}
	}
	optimum := 0
	for start := 0; start < len(pairs); {
		end := start + 1
		for end < len(pairs) && pairs[end].Old == pairs[start].Old {
			end++
		}
		for k := start; k < end; k++ {
			forward[k] = query(pairs[k].Next) + 1
			optimum = max(optimum, forward[k])
		}
		for k := start; k < end; k++ {
			update(pairs[k].Next+1, forward[k])
		}
		start = end
	}
	clear(tree)
	for end := len(pairs); end > 0; {
		start := end - 1
		for start > 0 && pairs[start-1].Old == pairs[end-1].Old {
			start--
		}
		for k := start; k < end; k++ {
			backward[k] = query(len(next)-pairs[k].Next-1) + 1
		}
		for k := start; k < end; k++ {
			update(len(next)-pairs[k].Next, backward[k])
		}
		end = start
	}
	counts := make([]int, optimum+1)
	for k, pair := range pairs {
		if forward[k]+backward[k]-1 == optimum {
			result.feasible = append(result.feasible, pair)
			counts[forward[k]]++
		}
	}
	for k, pair := range pairs {
		if forward[k]+backward[k]-1 == optimum && counts[forward[k]] == 1 {
			result.agreed = append(result.agreed, pair)
		}
	}
	return result
}
func physicalComparisonRows(rows []string) []string {
	result := make([]string, len(rows))
	for i, row := range rows {
		result[i] = physicalComparisonRow(row)
	}
	return result
}

const (
	physicalUnaccounted uint8 = iota
	physicalMatched
	physicalRetired
	physicalReplaced
	physicalHeldOutcome
)

type physicalTransition struct {
	outcome     []uint8
	consensus   physicalConsensusResult
	diagnostic  SnapshotAdmission
	agreedIndex []int
	header      int
}

type physicalAnchorRun struct{ oldStart, newStart, end, pairs int }

func (plan *physicalTransition) anchorRun(old, next []string, pair physicalPair) physicalAnchorRun {
	a, b := pair.Old, pair.Next
	for a > plan.header && b > plan.header && old[a-1] == next[b-1] && (!physicalSubstantive(old[a-1]) || plan.agreedIndex[a-1] == b-1) {
		a--
		b--
	}
	end, j, count := a, b, 0
	for end < len(old) && j < len(next) && old[end] == next[j] && (!physicalSubstantive(old[end]) || plan.agreedIndex[end] == j) {
		if plan.agreedIndex[end] == j {
			count++
		}
		end++
		j++
	}
	return physicalAnchorRun{a, b, end, count}
}

func planPhysicalTransition(old, next []string, linear bool) physicalTransition {
	result := physicalTransition{outcome: make([]uint8, len(old))}
	d := &result.diagnostic
	d.Class = "stationary"
	for _, row := range old {
		if row != "" {
			d.OldNonblank++
		}
	}
	if slices.Equal(old, next) {
		for i := range old {
			result.outcome[i] = physicalMatched
			if old[i] != "" {
				d.MatchedNonblank++
			}
		}
		d.MatchPath = "identical"
		d.MatchedRows = len(old)
		return result
	}
	result.consensus = physicalConsensus(old, next, linear)
	d.DPPairs = result.consensus.pairs
	d.MatchPath = result.consensus.path
	if result.consensus.budget {
		d.Class = "freeze"
		d.Reason = "pair_budget"
		d.HeldNonblank = d.OldNonblank
		for i := range result.outcome {
			result.outcome[i] = physicalHeldOutcome
		}
		return result
	}
	agreed := make([]int, len(old))
	for i := range agreed {
		agreed[i] = -1
	}
	feasible := make([]bool, len(old))
	for _, pair := range result.consensus.feasible {
		feasible[pair.Old] = true
	}
	for _, pair := range result.consensus.agreed {
		agreed[pair.Old] = pair.Next
		result.outcome[pair.Old] = physicalMatched
	}
	header := 0
	for header < min(len(old), len(next)) && old[header] == next[header] && (!physicalSubstantive(old[header]) || agreed[header] == header) {
		result.outcome[header] = physicalMatched
		header++
	}
	result.agreedIndex, result.header = agreed, header
	// A forward range needs an agreed contiguous anchor and a blank new gap.
	for _, pair := range result.consensus.agreed {
		if pair.Old < header {
			continue
		}
		run := result.anchorRun(old, next, pair)
		a, b, end := run.oldStart, run.newStart, run.end
		if run.pairs < 2 {
			continue
		}
		supported := b >= header
		for j := header; supported && j < b; j++ {
			supported = next[j] == ""
		}
		hasSub := false
		for i := header; supported && i < a; i++ {
			if physicalSubstantive(old[i]) {
				hasSub = true
				if feasible[i] {
					supported = false
				}
			}
		}
		if supported {
			retireEnd := a
			// Equal leading blanks admit later anchor boundaries with the same
			// substantive support. Retain their old-coordinate union once.
			for i, j := a, b; i < end && old[i] == "" && next[j] == ""; i, j = i+1, j+1 {
				retireEnd = i + 1
			}
			for i := header; i < retireEnd; i++ {
				if hasSub {
					result.outcome[i] = physicalRetired
				} else if old[i] != "" {
					result.outcome[i] = physicalHeldOutcome
				}
			}
			for i := a; i < end; i++ {
				if result.outcome[i] != physicalRetired {
					result.outcome[i] = physicalMatched
				}
			}
		}
		break
	}
	bounds := append([]physicalPair{{-1, -1}}, result.consensus.agreed...)
	bounds = append(bounds, physicalPair{len(old), len(next)})
	for k := 1; k < len(bounds); k++ {
		left, right := bounds[k-1], bounds[k]
		a, b, c, e := left.Old+1, right.Old, left.Next+1, right.Next
		// Remove already retired prefix coordinates without joining disjoint slots.
		for a < b && result.outcome[a] == physicalRetired {
			a++
		}
		for a < b && c < e && result.outcome[a] != physicalRetired && old[a] == next[c] && (!physicalSubstantive(old[a]) || !feasible[a]) {
			result.outcome[a] = physicalMatched
			a++
			c++
		}
		for a < b && c < e && result.outcome[b-1] != physicalRetired && old[b-1] == next[e-1] && (!physicalSubstantive(old[b-1]) || !feasible[b-1]) {
			b--
			e--
			result.outcome[b] = physicalMatched
		}
		if a >= b {
			continue
		}
		if b-a == e-c {
			for offset := 0; offset < b-a; offset++ {
				i, j := a+offset, c+offset
				if old[i] == next[j] && !physicalSubstantive(old[i]) && result.outcome[i] == physicalUnaccounted {
					result.outcome[i] = physicalMatched
				}
			}
		}
		supported := true
		for i := a; i < b; i++ {
			if result.outcome[i] != physicalUnaccounted && result.outcome[i] != physicalHeldOutcome && result.outcome[i] != physicalMatched || physicalSubstantive(old[i]) && feasible[i] {
				supported = false
			}
		}
		if !supported {
			continue
		}
		cleared := b-a == e-c && left.Old >= 0 && right.Old < len(old)
		for j := c; cleared && j < e; j++ {
			cleared = next[j] == ""
		}
		if cleared {
			for i := a; i < b; i++ {
				result.outcome[i] = physicalRetired
			}
			continue
		}
		// Leading missing rows need the forward anchor, not an in-place slot.
		if left.Old < 0 && c == e {
			continue
		}
		if b-a <= ReplaceableTailLines && e-c >= b-a && (left.Old >= 0 || len(result.consensus.agreed) == 0) {
			for i := a; i < b; i++ {
				if result.outcome[i] != physicalMatched {
					result.outcome[i] = physicalReplaced
				}
			}
		}
	}
	for i, row := range old {
		if result.outcome[i] == physicalUnaccounted && row != "" {
			result.outcome[i] = physicalHeldOutcome
		}
		switch result.outcome[i] {
		case physicalMatched:
			d.MatchedRows++
			if row != "" {
				d.MatchedNonblank++
			}
		case physicalRetired:
			if row != "" {
				d.RetiredNonblank++
			}
			d.RetiredRows++
		case physicalReplaced:
			if row != "" {
				d.ReplacedNonblank++
			}
			d.ReplacedRows++
		case physicalHeldOutcome:
			if row != "" {
				d.HeldNonblank++
			}
		}
	}
	switch {
	case d.HeldNonblank >= 3 && 2*d.HeldNonblank > d.OldNonblank && d.OldNonblank >= 8:
		d.Class = "freeze"
		d.Reason = "held_majority"
	case d.RetiredRows > 0:
		d.Class = "forward"
	case d.HeldNonblank > 0:
		d.Class = "held"
	case d.ReplacedRows > 0:
		d.Class = "replaced"
	}
	return result
}

func verifiedPhysicalReverse(state *PaneState, old, next []string, plan *physicalTransition) int {
	if state.LiveStart <= state.SegmentStart || plan.diagnostic.Class == "freeze" {
		return 0
	}

	header := plan.header
	processedEnd := -1
	for _, pair := range plan.consensus.agreed {
		if pair.Old < header || pair.Old < processedEnd || pair.Next-pair.Old < 2 {
			continue
		}
		run := plan.anchorRun(old, next, pair)
		processedEnd = run.end
		if run.oldStart != header || run.pairs < 2 {
			continue
		}
		k := run.newStart - run.oldStart
		if k > state.LiveStart-state.SegmentStart {
			return 0
		}
		archived := physicalComparisonRows(state.History[state.LiveStart-k : state.LiveStart])
		if !slices.Equal(archived, next[header:header+k]) {
			return 0
		}
		substantive := 0
		for _, row := range archived {
			if physicalSubstantive(row) {
				substantive++
			}
		}
		if substantive < 2 {
			return 0
		}
		combined := append(slices.Clone(archived), old[header:]...)
		proof := physicalConsensus(combined, next[header:], true)
		if proof.budget {
			return 0
		}
		prefixPairs := 0
		for _, p := range proof.agreed {
			if p.Old < k && p.Old == p.Next {
				prefixPairs++
			}
		}
		if prefixPairs >= 2 {
			return k
		}
	}

	return 0
}

func (m *Manager) mergePhysical(state *PaneState, body []string) SnapshotAdmission {
	previous := slices.Clone(state.History[state.LiveStart:])
	old, next := comparisonBody(previous), comparisonBody(body)
	plan := planPhysicalTransition(old, next, true)
	d := plan.diagnostic
	state.LastHash = hashLines(body)
	state.StaleRefusals = 0
	if len(previous) == 0 {
		d.Class = "initial"
		state.History = append(state.History[:state.LiveStart], body...)
		return d
	}
	if d.Class == "freeze" {
		state.LiveStart = len(state.History)
		state.History = append(state.History, body...)
		return d
	}
	retract := verifiedPhysicalReverse(state, old, next, &plan)
	archive := slices.Clone(state.History[:state.LiveStart-retract])
	pin := len(archive)
	for i := 0; i < len(old); {
		switch plan.outcome[i] {
		case physicalRetired:
			archive = append(archive, strings.Clone(previous[i]))
			pin++
			i++
		case physicalHeldOutcome, physicalUnaccounted:
			start := i
			for i < len(old) && (plan.outcome[i] == physicalHeldOutcome || plan.outcome[i] == physicalUnaccounted) {
				i++
			}
			end := i
			// Keep interior blank layout, not unrelated blank edges of the slot.
			for start < end && old[start] == "" {
				start++
			}
			for end > start && old[end-1] == "" {
				end--
			}
			d.HeldAddedRows += appendHeldRows(state, previous[start:end], pin, state.SegmentStart, m.now())
		default:
			i++
		}
	}
	state.LiveStart = len(archive)
	state.History = append(archive, body...)
	if retract > 0 {
		d.Class = "reverse"
		d.RetractedRows = retract
		d.RetractionCount = 1
		clampHeldPins(state)
	}
	return d
}
