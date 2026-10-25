package history

import (
	"slices"
	"strings"
	"time"
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
	NoScrollback  bool
}

type SnapshotAdmission struct {
	Admitted         bool
	ContentChanged   bool
	MetadataChanged  bool
	HeldEvictedRows  int
	HeldExpiredRows  int
	HeldTotalRows    int
	Class            string
	Reason           string
	MatchPath        string
	MatchedNonblank  int
	MatchedRows      int
	RetiredNonblank  int
	ReplacedNonblank int
	HeldNonblank     int
	OldNonblank      int
	RetiredRows      int
	ReplacedRows     int
	HeldAddedRows    int
	RetractedRows    int
	RetractionCount  int
	DPPairs          int
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

func (m *Manager) PreviewSnapshot(paneID, rawContent string, context SnapshotContext) (string, bool) {
	m.mu.Lock()
	state := m.loadState(paneID)
	if context.TerminalID == "" || state.TerminalID != context.TerminalID {
		m.mu.Unlock()
		return rawContent, false
	}
	if context.ReadSource == SourceRecent {
		rows := slices.Clone(state.History[:state.LiveStart])
		m.mu.Unlock()
		rows = append(rows, strings.Split(rawContent, "\n")...)
		content, _ := limitSnapshotLines(rows, 0)
		return content, true
	}
	clone := *state
	clone.History, clone.Footer = slices.Clone(state.History), slices.Clone(state.Footer)
	clone.Held = cloneHeldRecords(state.Held)
	clone.zeroDisplacementReturn = clonePhysicalReturn(state.zeroDisplacementReturn)
	m.mu.Unlock()
	preview := &Manager{states: map[string]*PaneState{paneID: &clone}, lastSave: make(map[string]time.Time), now: time.Now}
	content, _, _ := preview.AdmitSnapshot(paneID, rawContent, context, 0)
	return content, true
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
	beforeHeld := cloneHeldRecords(state.Held)
	beforeContext := state.snapshotContext()
	beforeVersion, beforeSegment, beforeLive := state.SnapshotVersion, state.SegmentStart, state.LiveStart
	beforeHash, beforeRefusals := state.LastHash, state.StaleRefusals
	if context.ReadSource != SourceRecent {
		context.ReadSource = SourceLegacy
	}
	if state.TerminalID != "" && context.TerminalID != "" && state.TerminalID != context.TerminalID {
		state.zeroDisplacementReturn = nil
		state = &PaneState{}
		m.states[paneID] = state
		delete(m.lastSave, paneID)
	}
	geometryBoundary := context.ReadSource == SourceRecent
	if context.GeometryEpoch == 0 && state.ReadSource == context.ReadSource && !state.loaded {
		context.GeometryEpoch, context.Columns, context.Rows = state.GeometryEpoch, state.Columns, state.Rows
	}
	legacyUpgrade := context.ReadSource == SourceLegacy && state.SnapshotVersion == 0 && !state.snapshotInvalid
	if legacyUpgrade {
		state.SegmentStart, state.LiveStart = 0, len(state.History)
	}
	boundary := geometryBoundary && state.loaded || state.SnapshotVersion != snapshotVersion && !legacyUpgrade ||
		state.ReadSource != context.ReadSource || geometryBoundary && state.GeometryEpoch != 0 && context.GeometryEpoch != 0 && state.GeometryEpoch != context.GeometryEpoch ||
		geometryBoundary && state.LeaseRevision != context.LeaseRevision
	if boundary {
		state.zeroDisplacementReturn = nil
		if state.LiveStart < len(state.History) {
			state.archiveMutated()
		}
		state.SegmentStart = len(state.History)
		state.LiveStart = len(state.History)
		state.LastHash = ""
		state.StaleRefusals = 0
	}
	if geometryBoundary {
		state.loaded = false
	}
	state.setSnapshotContext(context)
	body, footer := splitSnapshot(rawContent)
	state.Footer = slices.Clone(footer)
	admission := SnapshotAdmission{Class: "legacy"}
	if context.ReadSource == SourceRecent {
		admission = m.mergePhysical(state, body)
		if boundary {
			admission.Class = "boundary"
		}
	} else {
		m.mergeLegacySegment(state, body)
		if !slices.Equal(beforeHistory, state.History) {
			state.archiveMutated()
		}
		state.LiveStart = len(state.History)
	}
	expired := trimSnapshotState(state)
	evicted := trimHeldCapacity(state)
	admission.HeldExpiredRows, admission.HeldEvictedRows, admission.HeldTotalRows = expired, evicted, heldRowCount(state.Held)
	admission.Admitted = true
	admission.ContentChanged = !slices.Equal(beforeHistory, state.History) || !slices.Equal(beforeFooter, state.Footer)
	admission.MetadataChanged = beforeContext != state.snapshotContext() || beforeVersion != state.SnapshotVersion ||
		beforeSegment != state.SegmentStart || beforeLive != state.LiveStart || boundary || !equalHeldRecords(beforeHeld, state.Held)
	if admission.ContentChanged || admission.MetadataChanged || beforeHash != state.LastHash || beforeRefusals != state.StaleRefusals {
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
	state.snapshotInvalid = false
	if context.TerminalID != "" {
		state.TerminalID = context.TerminalID
	}
	state.ReadSource, state.GeometryEpoch, state.LeaseRevision = context.ReadSource, context.GeometryEpoch, context.LeaseRevision
	state.Columns, state.Rows = context.Columns, context.Rows
}

func LimitSnapshotContent(content string, limit int) (string, bool) {
	return limitSnapshotLines(strings.Split(content, "\n"), limit)
}

func limitSnapshotLines(lines []string, limit int) (string, bool) {
	clipped := limit > 0 && len(lines) > limit
	if clipped {
		lines = lines[len(lines)-limit:]
	}
	return strings.Join(lines, "\n"), clipped
}

func cloneHeldRecords(records []HeldRecord) []HeldRecord {
	clone := slices.Clone(records)
	for i := range clone {
		clone[i].Rows = slices.Clone(clone[i].Rows)
	}
	return clone
}

func equalHeldRecords(a, b []HeldRecord) bool {
	return slices.EqualFunc(a, b, func(a, b HeldRecord) bool {
		return a.Position == b.Position && a.Segment == b.Segment && a.FirstSeenAt.Equal(b.FirstSeenAt) && slices.Equal(a.Rows, b.Rows)
	})
}

func heldRowCount(records []HeldRecord) int {
	count := 0
	for _, record := range records {
		count += len(record.Rows)
	}
	return count
}

func clampHeldPins(state *PaneState) {
	for i := range state.Held {
		state.Held[i].Position = min(state.Held[i].Position, state.LiveStart)
		state.Held[i].Segment = min(state.Held[i].Segment, state.Held[i].Position)
	}
}

func appendHeldRows(state *PaneState, rows []string, position, segment int, now time.Time) int {
	nonblank := false
	for _, row := range rows {
		if physicalComparisonRow(row) != "" {
			nonblank = true
			break
		}
	}
	if !nonblank {
		return 0
	}
	owned := make([]string, len(rows))
	for i, row := range rows {
		owned[i] = strings.Clone(row)
	}
	state.Held = append(state.Held, HeldRecord{Rows: owned, Position: position, Segment: segment, FirstSeenAt: now.UTC()})
	return len(rows)
}

func trimHeldCapacity(state *PaneState) int {
	removed := max(0, heldRowCount(state.Held)-MaxHeldRows)
	remaining := removed
	for len(state.Held) > 0 && remaining >= len(state.Held[0].Rows) {
		remaining -= len(state.Held[0].Rows)
		state.Held[0] = HeldRecord{}
		state.Held = state.Held[1:]
	}
	if remaining > 0 {
		state.Held[0].Rows = slices.Clone(state.Held[0].Rows[remaining:])
	}
	return removed
}

func trimSnapshotState(state *PaneState) int {
	removed := max(0, len(state.History)-MaxLines)
	if removed > 0 {
		state.archiveMutated()
	}
	state.History = state.History[removed:]
	state.SegmentStart = min(len(state.History), max(0, state.SegmentStart-removed))
	state.LiveStart = min(len(state.History), max(state.SegmentStart, state.LiveStart-removed))
	expired := 0
	kept := state.Held[:0]
	for _, record := range state.Held {
		if record.Position < removed {
			expired += len(record.Rows)
			continue
		}
		record.Position -= removed
		record.Segment = max(0, record.Segment-removed)
		kept = append(kept, record)
	}
	clear(state.Held[len(kept):])
	state.Held = kept
	clampHeldPins(state)
	return expired
}

func validateSnapshotState(state *PaneState) {
	legacyFormat := state.SnapshotVersion == 0 && state.ReadSource == "" &&
		state.SegmentStart == 0 && state.LiveStart == 0 && state.GeometryEpoch == 0 &&
		state.LeaseRevision == 0 && state.Columns == 0 && state.Rows == 0
	validHeld := !state.heldInvalid && !state.snapshotInvalid && heldRowCount(state.Held) <= MaxHeldRows
	for _, record := range state.Held {
		if len(record.Rows) == 0 || record.FirstSeenAt.IsZero() || record.Segment < 0 || record.Position < record.Segment || record.Position > state.LiveStart {
			validHeld = false
		}
	}
	if validHeld && state.SnapshotVersion == snapshotVersion &&
		(state.ReadSource == SourceRecent || state.ReadSource == SourceLegacy) &&
		state.SegmentStart >= 0 && state.SegmentStart <= state.LiveStart && state.LiveStart <= len(state.History) {
		return
	}
	state.snapshotInvalid = !legacyFormat || !validHeld || len(state.Held) > 0
	state.Held = nil
	state.heldInvalid = false
	state.SnapshotVersion = 0
	state.ReadSource = SourceLegacy
	state.GeometryEpoch, state.LeaseRevision = 0, 0
	state.Columns, state.Rows = 0, 0
	state.SegmentStart, state.LiveStart = len(state.History), len(state.History)
}

func (m *Manager) mergeLegacySegment(state *PaneState, body []string) {
	active := &PaneState{
		History: state.History[state.SegmentStart:], LastHash: state.LastHash, StaleRefusals: state.StaleRefusals,
	}
	if !m.mergeLegacyBody(active, body) {
		return
	}
	prefix := slices.Clone(state.History[:state.SegmentStart])
	state.History = append(prefix, active.History...)
	state.LastHash, state.StaleRefusals = active.LastHash, active.StaleRefusals
}

func (m *Manager) mergeLegacyBody(state *PaneState, body []string) bool {
	if len(body) == 0 {
		return false
	}
	hash := hashLines(body)
	if hash == state.LastHash && state.StaleRefusals == 0 {
		return false
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
	return true
}
