package history

import (
	"slices"
	"strings"
)

const (
	PhysicalCaptureLines = 1000
	ReplaceableTailLines = 3
	SourceRecent         = "recent"
	SourceLegacy         = "legacy"
	snapshotVersion      = 1
)

type SnapshotContext struct {
	TerminalID    string
	ReadSource    string
	GeometryEpoch uint64
	LeaseRevision uint64
	Columns       int
	Rows          int
}

type SnapshotAdmission struct {
	Admitted        bool
	ContentChanged  bool
	MetadataChanged bool
}

// MergeSnapshot retains a page using its source and sampled PTY context.
func (m *Manager) MergeSnapshot(paneID, rawContent string, context SnapshotContext) string {
	content, _ := m.MergeSnapshotLimited(paneID, rawContent, context, 0)
	return content
}

func (m *Manager) MergeSnapshotLimited(paneID, rawContent string, context SnapshotContext, limit int) (string, bool) {
	content, clipped, _ := m.AdmitSnapshot(paneID, rawContent, context, limit)
	return content, clipped
}

// AdmitSnapshot reports admission separately from content and metadata changes.
func (m *Manager) AdmitSnapshot(paneID, rawContent string, context SnapshotContext, limit int) (string, bool, SnapshotAdmission) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.loadState(paneID)
	if rawContent == "" {
		content, clipped := joinContent(state, limit)
		return content, clipped, SnapshotAdmission{}
	}
	beforeHistory, beforeFooter := slices.Clone(state.History), slices.Clone(state.Footer)
	beforeContext := state.snapshotContext()
	beforeVersion, beforeSegment, beforeLive := state.SnapshotVersion, state.SegmentStart, state.LiveStart
	if context.ReadSource != SourceRecent {
		context.ReadSource = SourceLegacy
	}
	if state.TerminalID != "" && context.TerminalID != "" && state.TerminalID != context.TerminalID {
		state = &PaneState{}
		m.states[paneID] = state
		delete(m.lastSave, paneID)
	}
	boundary := state.loaded || state.SnapshotVersion != snapshotVersion ||
		state.ReadSource != context.ReadSource || state.GeometryEpoch != context.GeometryEpoch ||
		state.LeaseRevision != context.LeaseRevision
	if boundary {
		state.SegmentStart = len(state.History)
		state.LiveStart = len(state.History)
		state.LastHash = ""
		state.StaleRefusals = 0
	}
	state.loaded = false
	state.setSnapshotContext(context)
	body, footer := splitSnapshot(rawContent)
	state.Footer = slices.Clone(footer)
	if context.ReadSource == SourceRecent {
		m.mergePhysical(state, body)
	} else {
		m.mergeLegacySegment(state, body)
		state.LiveStart = len(state.History)
	}
	trimSnapshotState(state)
	admission := SnapshotAdmission{
		Admitted:       true,
		ContentChanged: !slices.Equal(beforeHistory, state.History) || !slices.Equal(beforeFooter, state.Footer),
		MetadataChanged: beforeContext != state.snapshotContext() || beforeVersion != state.SnapshotVersion ||
			beforeSegment != state.SegmentStart || beforeLive != state.LiveStart || boundary,
	}
	if admission.ContentChanged || admission.MetadataChanged {
		state.dirty = true
		state.mutationVersion++
	}
	m.maybeSave(paneID, state)
	if context.ReadSource == SourceRecent {
		// Response clipping uses the raw page, not a sliced reconstruction of the live body.
		prefix := state.History[:state.LiveStart]
		lines := append(slices.Clone(prefix), strings.Split(rawContent, "\n")...)
		returnContent, clipped := limitSnapshotLines(lines, limit)
		return returnContent, clipped, admission
	}
	content, clipped := joinContent(state, limit)
	return content, clipped, admission
}

func (state *PaneState) snapshotContext() SnapshotContext {
	return SnapshotContext{
		TerminalID: state.TerminalID, ReadSource: state.ReadSource, GeometryEpoch: state.GeometryEpoch,
		LeaseRevision: state.LeaseRevision, Columns: state.Columns, Rows: state.Rows,
	}
}

func (state *PaneState) setSnapshotContext(context SnapshotContext) {
	state.SnapshotVersion = snapshotVersion
	if context.TerminalID != "" {
		state.TerminalID = context.TerminalID
	}
	state.ReadSource, state.GeometryEpoch, state.LeaseRevision = context.ReadSource, context.GeometryEpoch, context.LeaseRevision
	state.Columns, state.Rows = context.Columns, context.Rows
}

func limitSnapshotLines(lines []string, limit int) (string, bool) {
	clipped := limit > 0 && len(lines) > limit
	if clipped {
		lines = lines[len(lines)-limit:]
	}
	return strings.Join(lines, "\n"), clipped
}

func trimSnapshotState(state *PaneState) {
	removed := max(0, len(state.History)-MaxLines)
	state.History = state.History[removed:]
	state.SegmentStart = min(len(state.History), max(0, state.SegmentStart-removed))
	state.LiveStart = min(len(state.History), max(state.SegmentStart, state.LiveStart-removed))
}

func validateSnapshotState(state *PaneState) {
	if state.SnapshotVersion == snapshotVersion &&
		(state.ReadSource == SourceRecent || state.ReadSource == SourceLegacy) &&
		state.SegmentStart >= 0 && state.SegmentStart <= state.LiveStart && state.LiveStart <= len(state.History) {
		return
	}
	state.SnapshotVersion = 0
	state.ReadSource = SourceLegacy
	state.GeometryEpoch, state.LeaseRevision = 0, 0
	state.Columns, state.Rows = 0, 0
	state.SegmentStart, state.LiveStart = len(state.History), len(state.History)
}

func (m *Manager) mergeLegacySegment(state *PaneState, body []string) {
	prefix := slices.Clone(state.History[:state.SegmentStart])
	active := &PaneState{
		History: state.History[state.SegmentStart:], LastHash: state.LastHash, StaleRefusals: state.StaleRefusals,
	}
	m.mergeLegacyBody(active, body)
	state.History = append(prefix, active.History...)
	state.LastHash, state.StaleRefusals = active.LastHash, active.StaleRefusals
}

func (m *Manager) mergeLegacyBody(state *PaneState, body []string) {
	if len(body) == 0 {
		return
	}
	hash := hashLines(body)
	if hash == state.LastHash && state.StaleRefusals == 0 {
		return
	}
	state.LastHash = hash
	normalized, histNorm := normalizeLines(body), normalizeLines(state.History)
	if overlap := tailOverlap(histNorm, normalized); overlap > 0 {
		historyStart := len(state.History) - overlap
		copy(state.History[historyStart:], body[:overlap])
		state.History = append(state.History, body[overlap:]...)
	} else if match := sequenceMatch(histNorm, normalized); match.Size >= 2 {
		m.applyMatch(state, body, match)
	} else {
		state.History = append(state.History, body...)
		state.StaleRefusals = 0
	}
}

func (m *Manager) mergePhysical(state *PaneState, body []string) {
	previous := state.History[state.LiveStart:]
	old, next := normalizeLines(previous), normalizeLines(body)
	archive := state.History[:state.LiveStart]
	fallback := func() {
		state.LiveStart = len(state.History)
		state.History = append(state.History, body...)
	}
	state.LastHash = hashLines(body)
	state.StaleRefusals = 0
	if len(previous) == 0 || slices.Equal(old, next) {
		state.History = append(archive, body...)
		return
	}
	if len(next) < len(old) && slices.Equal(old[:len(next)], next) {
		fallback()
		return
	}
	reverse, _ := physicalAlignments(next, old)
	for _, candidate := range reverse {
		if candidate.displacement > 0 {
			fallback()
			return
		}
	}
	candidates, weak := physicalAlignments(old, next)
	if len(candidates) > 0 {
		candidate, conflict := choosePhysicalAlignment(candidates)
		if conflict {
			fallback()
			return
		}
		displaced := slices.Clone(previous[candidate.header : candidate.header+candidate.displacement])
		state.History = append(archive, displaced...)
		state.LiveStart = len(state.History)
		state.History = append(state.History, body...)
		return
	}
	if !weak && streamingExtension(old, next) {
		state.History = append(archive, body...)
		return
	}
	fallback()
}

// physicalAlignment excludes tail rows from old-page matching and archives its displaced range.
type physicalAlignment struct {
	header       int
	displacement int
	tail         int
}

func physicalAlignments(old, next []string) ([]physicalAlignment, bool) {
	prefix := 0
	for prefix < min(len(old), len(next)) && old[prefix] == next[prefix] {
		prefix++
	}
	var candidates []physicalAlignment
	weak := false
	for header := 0; header <= prefix; header++ {
		for tail := 0; tail <= min(ReplaceableTailLines, len(old)); tail++ {
			end := len(old) - tail
			for displacement := 0; header+displacement < end; displacement++ {
				span := old[header+displacement : end]
				if header+len(span) > len(next) || !slices.Equal(span, next[header:header+len(span)]) {
					continue
				}
				if displacement == 0 {
					newTail := len(next) - header - len(span)
					if tail == 0 || len(old) != len(next) || newTail <= 0 || newTail > ReplaceableTailLines ||
						slices.Equal(old[end:], next[header+len(span):]) {
						continue
					}
				}
				nonempty := 0
				for _, row := range span {
					if row != "" {
						nonempty++
					}
				}
				if nonempty < 2 {
					weak = weak || displacement > 0
					continue
				}
				candidates = append(candidates, physicalAlignment{header: header, displacement: displacement, tail: tail})
			}
		}
	}
	return candidates, weak
}

func choosePhysicalAlignment(candidates []physicalAlignment) (physicalAlignment, bool) {
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		sameRange := best.displacement == 0 && candidate.displacement == 0 ||
			best.header == candidate.header && best.displacement == candidate.displacement
		if !sameRange {
			return physicalAlignment{}, true
		}
		if candidate.tail < best.tail || candidate.tail == best.tail &&
			(candidate.displacement < best.displacement || candidate.displacement == best.displacement && candidate.header < best.header) {
			best = candidate
		}
	}
	return best, false
}

func streamingExtension(old, next []string) bool {
	if len(next) >= len(old) && slices.Equal(old, next[:len(old)]) {
		return true
	}
	if len(old) != len(next) || len(old) == 0 {
		return false
	}
	last := len(old) - 1
	for last >= 0 && old[last] == "" {
		last--
	}
	return last >= 0 && slices.Equal(old[:last], next[:last]) && slices.Equal(old[last+1:], next[last+1:]) &&
		len(next[last]) > len(old[last]) && strings.HasPrefix(next[last], old[last])
}
