package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

type historyIdentity struct {
	terminalID string
	generation int64
}

// historyReadToken belongs to an underlying read, not its single-flight followers.
type historyReadToken struct {
	paneID          string
	identity        historyIdentity
	sequence        uint64
	leaseRevision   uint64
	geometry        panesize.HistoryGeometry
	pendingSequence uint64
}

func (s *Server) physicalHistorySource(agent *coordinator.AgentState) (bool, uint64) {
	if agent == nil || s.paneSizeM == nil || !s.paneSizeM.Available() {
		return false, 0
	}
	leased, revision := s.paneSizeM.LeaseHistoryState(agent.PaneID, agent.TerminalID)
	return leased && agent.UsesNoScrollbackHistory(), revision
}

func (s *Server) readPaneResponse(ctx context.Context, message map[string]any) map[string]any {
	paneID, _ := message["pane_id"].(string)
	agent, _ := s.state.Agent(paneID)
	physical, leaseRevision := s.physicalHistorySource(agent)
	response, _ := s.dispatcher.HandleReadPaneWithOptions(ctx, message, coordinator.PaneReadOptions{
		Physical: physical, SourceRevision: leaseRevision,
		Execute: s.executeHistoryRead,
		ResizedRecently: func() bool {
			return s.paneSizeM != nil && s.paneSizeM.ResizedWithin(paneID, paneResizeSettleWindow)
		},
	})
	s.prepareAcceptedPaneInteraction(paneID, response)
	if target, ok := message["target"]; ok {
		response["target"] = target
	}
	return response
}

func (s *Server) prepareAcceptedPaneInteraction(paneID string, response map[string]any) {
	if response["error"] != nil {
		return
	}
	interaction, _ := response["interaction"].(*question.Interaction)
	if interaction == nil {
		return
	}
	interaction = clonePaneInteraction(interaction)
	response["interaction"] = interaction
	if text := strings.TrimSpace(interaction.Other.Text); text != "" {
		s.state.RecordCustomAnswer(paneID, interaction.Question, text)
	}
	question.FillCustomAnswers(interaction, s.state.CustomAnswers(paneID))
}

func clonePaneInteraction(interaction *question.Interaction) *question.Interaction {
	if interaction == nil {
		return nil
	}
	clone := *interaction
	clone.Options = slices.Clone(interaction.Options)
	for i := range clone.Options {
		clone.Options[i].Summary = slices.Clone(interaction.Options[i].Summary)
	}
	return &clone
}

func historyContentContext(ctx context.Context, background bool) (context.Context, context.CancelFunc) {
	if background {
		return context.WithTimeout(ctx, 3*time.Second)
	}
	return ctx, func() {}
}

func (s *Server) executeHistoryRead(ctx context.Context, request coordinator.PaneReadRequest,
	readContent func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error),
) (coordinator.PaneReadResult, error) {
	scrollMetricsRevision := s.state.ScrollMetricsRevision(request.PaneID)
	agent := request.Agent
	if agent == nil || !isClaudeLike(agent.Agent) {
		contentCtx, cancel := historyContentContext(ctx, request.Background)
		defer cancel()
		read, err := readContent(contentCtx, request)
		if err != nil {
			return coordinator.PaneReadResult{}, err
		}
		return coordinator.PaneReadResult{Read: read, Response: s.classifyRawPane(request.PaneID, string(read.Content), request.Format)}, nil
	}
	token := historyReadToken{paneID: request.PaneID}
	token.identity = historyIdentity{terminalID: agent.TerminalID, generation: request.Generation}
	s.historyPendingMu.Lock()
	if s.historyPendingClosed {
		s.historyPendingMu.Unlock()
		return coordinator.PaneReadResult{}, context.Canceled
	}
	s.historySequence++
	token.sequence = s.historySequence
	s.historyPendingMu.Unlock()
	physicalAtStart, leaseRevision := s.physicalHistorySource(agent)
	token.leaseRevision = leaseRevision
	var handle *panesize.HistoryRead
	var geometryErr error
	captureShape := request.Format == "ansi" && (request.Physical || !request.Resized)
	if captureShape && s.paneSizeM != nil && s.paneSizeM.Available() && agent.TerminalID != "" {
		handle, geometryErr = s.paneSizeM.BeginHistoryRead(ctx, request.PaneID, agent.TerminalID)
		if handle != nil {
			defer handle.Close()
		}
	}
	retainedUnsafe := s.paneSizeM != nil && s.paneSizeM.HistoryReadUnsafe(request.PaneID, token.identity.terminalID)
	if captureShape {
		s.historyPendingMu.Lock()
		var expired bool
		token.pendingSequence, expired = s.pendingReadStartedLocked(request.PaneID, token.identity)
		s.historyPendingMu.Unlock()
		if expired {
			s.logger.Warn("trailing pane capture expired", "pane_id", request.PaneID)
		}
	}
	contentCtx, cancel := historyContentContext(ctx, request.Background)
	defer cancel()
	read, err := readContent(contentCtx, request)
	if err != nil {
		return coordinator.PaneReadResult{}, err
	}
	if handle != nil {
		token.geometry, geometryErr = handle.Finish()
	}
	current, exists := s.state.Agent(request.PaneID)
	terminalCurrent := exists && current.TerminalID == token.identity.terminalID
	if handle == nil && (errors.Is(geometryErr, panesize.ErrTerminalGone) || errors.Is(geometryErr, panesize.ErrHistoryReadUnsafe)) {
		terminalCurrent = false
	}
	identityCurrent := terminalCurrent && s.state.Generation(request.PaneID) == token.identity.generation
	physicalNow, revisionNow := s.physicalHistorySource(current)
	if handle != nil {
		physicalNow, revisionNow = token.geometry.Leased && current != nil && current.UsesNoScrollbackHistory(), token.geometry.LeaseRevision
	}
	stablePolicy := physicalNow == physicalAtStart && revisionNow == token.leaseRevision && request.Physical == physicalAtStart &&
		s.state.ScrollMetricsRevision(request.PaneID) == scrollMetricsRevision && current != nil && agent.UsesNoScrollbackHistory() == current.UsesNoScrollbackHistory()
	raw := string(read.Content)
	layoutHint := question.LayoutHint(raw)
	response := map[string]any{"content": raw, "format": request.Format}
	if !request.Background {
		response = s.classifyRawPane(request.PaneID, raw, request.Format)
	}
	result := coordinator.PaneReadResult{Read: read, Response: response,
		Capture: coordinator.PaneReadCapture{Sequence: token.sequence, TerminalID: token.identity.terminalID, Generation: token.identity.generation}}
	if handle != nil && geometryErr != nil {
		response["resize_settling"] = true

	}
	interaction, _ := response["interaction"].(*question.Interaction)
	eligible := request.Format == "ansi" &&
		len(read.Content) > 0 && interaction == nil && !layoutHint &&
		identityCurrent && stablePolicy && ctx.Err() == nil && s.state.ContentRevision(request.PaneID) == request.ContentRevision
	if request.Physical {
		eligible = eligible && handle != nil && geometryErr == nil && token.geometry.Known && token.geometry.Leased
	} else {
		if handle == nil && s.paneSizeM != nil {
			retainedUnsafe = retainedUnsafe || s.paneSizeM.HistoryReadUnsafe(request.PaneID, token.identity.terminalID)
		}
		eligible = eligible && !request.Resized && !retainedUnsafe && (handle == nil || geometryErr == nil)
	}
	snapshotContext := history.SnapshotContext{TerminalID: token.identity.terminalID, ReadSource: history.SourceLegacy,
		GeometryEpoch: token.geometry.Epoch, LeaseRevision: token.leaseRevision,
		Columns: token.geometry.Columns, Rows: token.geometry.Rows, NoScrollback: agent.UsesNoScrollbackHistory()}
	if request.Physical {
		snapshotContext.ReadSource = history.SourceRecent
	}
	preview := func() {
		if !captureShape || interaction != nil || !terminalCurrent || len(read.Content) == 0 {
			return
		}
		active, ok := s.historyActive[request.PaneID]
		if !ok || active.terminalID != token.identity.terminalID {
			return
		}
		if content, composed := s.historyM.PreviewSnapshot(request.PaneID, raw, snapshotContext); composed {
			response["content"] = content
			result.Composed = true
		}
		if !(request.Physical && geometryErr != nil) {
			response["history_retry"] = true
		}
	}
	if !eligible {
		s.historyCaptureMu.Lock()
		preview()
		s.historyCaptureMu.Unlock()
		return result, nil
	}
	result.Capture.Snapshot = snapshotContext
	result.Capture.PendingSequence = token.pendingSequence
	if s.historyReadFinishedObserver != nil {
		s.historyReadFinishedObserver(token)
	}
	s.historyCaptureMu.Lock()
	var diagnostic *history.SnapshotAdmission
	defer func() {
		s.historyCaptureMu.Unlock()
		if diagnostic != nil {
			s.logHistoryAdmission(token, *diagnostic)
		}
	}()
	active, ok := s.historyActive[request.PaneID]
	if !ok || active != token.identity || token.sequence <= s.historyArchived[request.PaneID] || ctx.Err() != nil || s.historyClosed {
		preview()
		return result, nil
	}
	content, _, admission := s.historyM.AdmitSnapshot(request.PaneID, raw, snapshotContext, 0)
	if s.historyAdmissionObserver != nil {
		s.historyAdmissionObserver(token, admission)
	}
	if admission.Admitted {
		if snapshotContext.ReadSource == history.SourceRecent {
			s.recordHistoryAdmissionLocked(token, admission)
		}
		diagnostic = &admission
		response["content"] = content
		result.Capture.Admitted = true
		s.historyArchived[request.PaneID] = token.sequence
		s.historyPendingMu.Lock()
		s.acknowledgeHistoryLocked(token)
		s.historyPendingMu.Unlock()
	}
	return result, nil
}

func (s *Server) classifyRawPane(paneID, content, format string) map[string]any {
	agent, _ := s.agentInfo(paneID)
	classification := question.Classify(content, agent)
	response := map[string]any{"content": content, "attention_kind": classification.Kind,
		"prompt": classification.Prompt, "command": classification.Command, "options": classification.Options,
		"interaction": classification.Interaction, "question_layout": classification.QuestionLayout, "format": format}
	return response
}

func (s *Server) captureTerminalHistory(ctx context.Context, paneID string) (string, error) {
	agent, _ := s.state.Agent(paneID)
	physical, _ := s.physicalHistorySource(agent)
	lines := history.MaxLines
	if physical {
		lines = history.PhysicalCaptureLines
	}
	request := coordinator.PaneReadRequest{PaneID: paneID, Agent: agent, Generation: s.state.Generation(paneID),
		ContentRevision: s.state.ContentRevision(paneID), Format: "ansi", Lines: lines, Physical: physical, Background: true}
	result, err := s.executeHistoryRead(ctx, request, func(ctx context.Context, request coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		if request.Physical {
			return s.herdrC.ReadPaneRecent(ctx, paneID, request.Lines, "ansi")
		}
		return s.herdrC.ReadPane(ctx, paneID, request.Lines, "ansi")
	})
	if content, ok := result.Response["content"].(string); ok {
		return content, err
	}
	return string(result.Read.Content), err
}

type historyMinuteCounters struct {
	started                                                                                      time.Time
	admissions, initial, boundary, stationary, replaced, forward, held, reverse, legacy, freezes int
	heldMajority, pairBudget                                                                     int
	matchedRows, retiredRows, replacedRows, heldAddedRows, heldEvictedRows, heldExpiredRows      int
	retractionEvents, retractedRows, dpPairs, maxDPPairs, currentHeldRows                        int
}

func (s *Server) recordHistoryAdmissionLocked(token historyReadToken, d history.SnapshotAdmission) {
	if s.historyCounters == nil {
		s.historyCounters = make(map[string]*historyMinuteCounters)
	}
	counter := s.historyCounters[token.paneID]
	if counter == nil {
		counter = &historyMinuteCounters{started: s.historyNow()}
		s.historyCounters[token.paneID] = counter
	}
	counter.admissions++
	switch d.Class {
	case "initial":
		counter.initial++
	case "boundary":
		counter.boundary++
	case "stationary":
		counter.stationary++
	case "replaced":
		counter.replaced++
	case "forward":
		counter.forward++
	case "held":
		counter.held++
	case "reverse":
		counter.reverse++
	case "legacy":
		counter.legacy++
	case "freeze":
		counter.freezes++
		if d.Reason == "held_majority" {
			counter.heldMajority++
		}
		if d.Reason == "pair_budget" {
			counter.pairBudget++
		}
	}
	counter.matchedRows += d.MatchedRows
	counter.retiredRows += d.RetiredRows
	counter.replacedRows += d.ReplacedRows
	counter.heldAddedRows += d.HeldAddedRows
	counter.heldEvictedRows += d.HeldEvictedRows
	counter.heldExpiredRows += d.HeldExpiredRows
	counter.retractionEvents += d.RetractionCount
	counter.retractedRows += d.RetractedRows
	counter.dpPairs += d.DPPairs
	counter.maxDPPairs = max(counter.maxDPPairs, d.DPPairs)
	counter.currentHeldRows = d.HeldTotalRows
}

func (s *Server) logHistoryAdmission(token historyReadToken, d history.SnapshotAdmission) {
	if d.Class == "freeze" {
		share := 0.0
		if d.OldNonblank > 0 {
			share = float64(d.HeldNonblank) / float64(d.OldNonblank)
		}
		s.logger.Warn("physical history discontinuity", "pane_id", token.paneID, "sequence", token.sequence, "source", history.SourceRecent,
			"pty_epoch", token.geometry.Epoch, "lease_revision", token.leaseRevision, "reason", d.Reason,
			"held_nonblank", d.HeldNonblank, "old_nonblank", d.OldNonblank, "matched_nonblank", d.MatchedNonblank,
			"replaced_nonblank", d.ReplacedNonblank, "retired_nonblank", d.RetiredNonblank,
			"matched_rows", d.MatchedRows, "replaced_rows", d.ReplacedRows, "retired_rows", d.RetiredRows, "held_share", share, "dp_pairs", d.DPPairs)
	}
	if d.HeldEvictedRows > 0 {
		s.logger.Warn("physical history held rows removed", "pane_id", token.paneID, "reason", "held_capacity", "held_evicted_rows", d.HeldEvictedRows)
	}
	if d.HeldExpiredRows > 0 {
		s.logger.Warn("physical history held rows removed", "pane_id", token.paneID, "reason", "archive_pin_expiry", "held_expired_rows", d.HeldExpiredRows)
	}
}

func (s *Server) flushHistoryCounters(now time.Time) {
	type report struct {
		paneID   string
		counters historyMinuteCounters
	}
	var reports []report
	s.historyCaptureMu.Lock()
	for paneID, counter := range s.historyCounters {
		if now.Sub(counter.started) < time.Minute {
			continue
		}
		reports = append(reports, report{paneID, *counter})
		s.historyCounters[paneID] = &historyMinuteCounters{started: now, currentHeldRows: counter.currentHeldRows}
	}
	s.historyCaptureMu.Unlock()
	for _, report := range reports {
		c := report.counters
		s.logger.Info("physical history minute", "pane_id", report.paneID, "admissions", c.admissions,
			"initial", c.initial, "boundary", c.boundary, "stationary", c.stationary, "replaced", c.replaced, "forward", c.forward, "held", c.held, "reverse", c.reverse, "legacy", c.legacy,
			"freezes", c.freezes, "held_majority", c.heldMajority, "pair_budget", c.pairBudget,
			"matched_rows", c.matchedRows, "retired_rows", c.retiredRows, "replaced_rows", c.replacedRows,
			"held_added_rows", c.heldAddedRows, "held_evicted_rows", c.heldEvictedRows, "held_expired_rows", c.heldExpiredRows, "held_total_rows", c.currentHeldRows,
			"retraction_events", c.retractionEvents, "retracted_rows", c.retractedRows, "dp_pairs", c.dpPairs, "max_dp_pairs", c.maxDPPairs)
	}
}
