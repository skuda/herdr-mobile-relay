package app

import (
	"context"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/noecho"
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
	physical        bool
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
	})
	if target, ok := message["target"]; ok {
		response["target"] = target
	}
	return fitPaneContentResponse(response)
}

func (s *Server) executeHistoryRead(ctx context.Context, request coordinator.PaneReadRequest,
	readContent func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error),
) (coordinator.PaneReadResult, error) {
	scrollMetricsRevision := s.state.ScrollMetricsRevision(request.PaneID)
	agent := request.Agent
	token := historyReadToken{paneID: request.PaneID, physical: request.Physical}
	if agent != nil {
		token.identity = historyIdentity{terminalID: agent.TerminalID, generation: request.Generation}
	}
	s.historyCaptureMu.Lock()
	if s.historyClosed {
		s.historyCaptureMu.Unlock()
		return coordinator.PaneReadResult{}, context.Canceled
	}
	s.historySequence++
	token.sequence = s.historySequence
	s.historyCaptureMu.Unlock()
	physicalAtStart, leaseRevision := s.physicalHistorySource(agent)
	token.leaseRevision = leaseRevision
	var handle *panesize.HistoryRead
	var geometryErr error
	if s.paneSizeM != nil && s.paneSizeM.Available() && agent != nil && isClaudeLike(agent.Agent) && agent.TerminalID != "" {
		handle, geometryErr = s.paneSizeM.BeginHistoryRead(ctx, request.PaneID, agent.TerminalID)
		if handle != nil {
			defer handle.Close()
		}
	}
	retainedUnsafe := s.paneSizeM != nil && s.paneSizeM.HistoryReadUnsafe(request.PaneID, token.identity.terminalID)
	s.historyCaptureMu.Lock()
	token.pendingSequence = s.pendingReadStartedLocked(request.PaneID, token.identity)
	s.historyCaptureMu.Unlock()
	read, err := readContent(ctx, request)
	if err != nil {
		return coordinator.PaneReadResult{}, err
	}
	if handle != nil {
		token.geometry, geometryErr = handle.Finish()
	}
	current, exists := s.state.Agent(request.PaneID)
	identityCurrent := exists && current.TerminalID == token.identity.terminalID && s.state.Generation(request.PaneID) == token.identity.generation
	physicalNow, revisionNow := s.physicalHistorySource(current)
	if handle != nil {
		physicalNow, revisionNow = token.geometry.Leased && current != nil && current.UsesNoScrollbackHistory(), token.geometry.LeaseRevision
	}
	stablePolicy := physicalNow == physicalAtStart && revisionNow == token.leaseRevision && request.Physical == physicalAtStart &&
		s.state.ScrollMetricsRevision(request.PaneID) == scrollMetricsRevision && agent != nil && current != nil && agent.UsesNoScrollbackHistory() == current.UsesNoScrollbackHistory()
	response := s.classifyRawPane(request.PaneID, string(read.Content), request.Format)
	result := coordinator.PaneReadResult{Read: read, Response: response,
		Capture: coordinator.PaneReadCapture{Sequence: token.sequence, TerminalID: token.identity.terminalID, Generation: token.identity.generation}}
	if handle != nil && geometryErr != nil {
		response["resize_settling"] = true
	} else if request.Resized && s.paneSizeM != nil && s.paneSizeM.ResizedWithin(request.PaneID, paneResizeSettleWindow) {
		response["resize_settling"] = true
	}
	interaction, _ := response["interaction"].(*question.Interaction)
	eligible := agent != nil && isClaudeLike(agent.Agent) && request.Format == "ansi" &&
		len(read.Content) > 0 && interaction == nil && !question.LayoutHint(string(read.Content)) &&
		identityCurrent && stablePolicy && ctx.Err() == nil && s.state.ContentRevision(request.PaneID) == request.ContentRevision
	if request.Physical {
		eligible = eligible && handle != nil && geometryErr == nil && token.geometry.Known && token.geometry.Leased
	} else {
		if handle == nil && s.paneSizeM != nil {
			retainedUnsafe = retainedUnsafe || s.paneSizeM.HistoryReadUnsafe(request.PaneID, token.identity.terminalID)
		}
		eligible = eligible && !request.Resized && !retainedUnsafe && (handle == nil || geometryErr == nil)
	}
	if !eligible {
		return result, nil
	}
	context := history.SnapshotContext{TerminalID: token.identity.terminalID, ReadSource: history.SourceLegacy,
		GeometryEpoch: token.geometry.Epoch, LeaseRevision: token.leaseRevision,
		Columns: token.geometry.Columns, Rows: token.geometry.Rows}
	if request.Physical {
		context.ReadSource = history.SourceRecent
	}
	result.Capture.Snapshot = context
	result.Capture.PendingSequence = token.pendingSequence
	if s.historyReadFinishedObserver != nil {
		s.historyReadFinishedObserver(token)
	}
	s.historyCaptureMu.Lock()
	defer s.historyCaptureMu.Unlock()
	active, ok := s.historyActive[request.PaneID]
	if !ok || active != token.identity || token.sequence <= s.historyArchived[request.PaneID] || ctx.Err() != nil || s.historyClosed {
		return result, nil
	}
	content, _, admission := s.historyM.AdmitSnapshot(request.PaneID, string(read.Content), context, 0)
	if s.historyAdmissionObserver != nil {
		s.historyAdmissionObserver(token, admission)
	}
	if admission.Admitted {
		response["content"] = content
		result.Capture.Admitted = true
		s.historyArchived[request.PaneID] = token.sequence
		s.acknowledgeHistoryLocked(token)
	}
	return result, nil
}

func (s *Server) classifyRawPane(paneID, content, format string) map[string]any {
	agent, _ := s.agentInfo(paneID)
	classification := question.Classify(content, agent)
	response := map[string]any{"content": content, "attention_kind": classification.Kind,
		"prompt": classification.Prompt, "command": classification.Command, "options": classification.Options,
		"interaction": classification.Interaction, "question_layout": classification.QuestionLayout, "format": format}
	if interaction := classification.Interaction; interaction != nil {
		if text := strings.TrimSpace(interaction.Other.Text); text != "" {
			s.state.RecordCustomAnswer(paneID, interaction.Question, text)
		}
		question.FillCustomAnswers(interaction, s.state.CustomAnswers(paneID))
	}
	prompt, secret := noecho.Match(content)
	response["no_echo"] = secret
	if secret {
		response["no_echo_prompt"] = prompt
	}
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
		ContentRevision: s.state.ContentRevision(paneID), Format: "ansi", Lines: lines, Physical: physical}
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
