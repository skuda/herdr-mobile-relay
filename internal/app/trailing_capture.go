package app

import (
	"context"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
)

const (
	trailingCaptureAttempts = 5
	trailingCaptureLifetime = 60 * time.Second
)

type pendingHistoryCapture struct {
	identity historyIdentity
	sequence uint64
	revision int64
	queued   time.Time
	attempts int
}

// enqueueHistoryTransition records accepted departures without coordinator lookups or pane reads.
func (s *Server) enqueueHistoryTransition(event coordinator.StatusTransition) {
	s.historyCaptureMu.Lock()
	defer s.historyCaptureMu.Unlock()
	if s.historyClosed {
		return
	}
	if event.Revision <= s.historyStatusRevision[event.PaneID] {
		return
	}
	if active, ok := s.historyActive[event.PaneID]; ok && (event.Generation < active.generation || event.Generation == active.generation && active.terminalID != event.TerminalID) {
		return
	}
	if event.OldStatus != "working" && event.OldStatus != "blocked" || event.NewStatus == "working" || event.NewStatus == "blocked" {
		return
	}
	s.historyStatusRevision[event.PaneID] = event.Revision
	if existing := s.historyPending[event.PaneID]; existing != nil && existing.identity.terminalID == event.TerminalID {
		s.historyRequestSequence++
		existing.sequence = s.historyRequestSequence
		existing.revision = event.Revision
		return
	}
	s.historyRequestSequence++
	s.historyPending[event.PaneID] = &pendingHistoryCapture{identity: historyIdentity{terminalID: event.TerminalID, generation: event.Generation},
		sequence: s.historyRequestSequence, revision: event.Revision, queued: s.historyNow()}
}

func (s *Server) captureHistoryTick(now time.Time) {
	agents := s.state.Snapshot()
	s.syncHistoryPanes(agents)
	candidates := make(map[string]bool)
	for _, agent := range agents {
		if isClaudeLike(agent.Agent) && (agent.Status == "working" || agent.Status == "blocked") {
			candidates[agent.PaneID] = true
		}
	}
	s.historyCaptureMu.Lock()
	for paneID, pending := range s.historyPending {
		if now.Sub(pending.queued) >= trailingCaptureLifetime || pending.attempts >= trailingCaptureAttempts {
			delete(s.historyPending, paneID)
			s.logger.Warn("trailing pane capture expired", "pane_id", paneID)
			continue
		}
		candidates[paneID] = true
	}
	s.historyCaptureMu.Unlock()
	for paneID := range candidates {
		s.scheduleHistoryCapture(context.Background(), paneID)
	}
}

func (s *Server) pendingReadStartedLocked(paneID string, identity historyIdentity) uint64 {
	pending := s.historyPending[paneID]
	if pending == nil || pending.identity != identity {
		return 0
	}
	if pending.attempts >= trailingCaptureAttempts || s.historyNow().Sub(pending.queued) >= trailingCaptureLifetime {
		delete(s.historyPending, paneID)
		return 0
	}
	pending.attempts++
	return pending.sequence
}

func (s *Server) acknowledgeHistoryLocked(token historyReadToken) {
	if pending := s.historyPending[token.paneID]; pending != nil && pending.identity == token.identity && pending.sequence == token.pendingSequence {
		delete(s.historyPending, token.paneID)
		s.historyAcknowledgements++
	}
}

func (s *Server) historyCaptureDeferred(ctx context.Context, paneID string) bool {
	agent, ok := s.state.Agent(paneID)
	if !ok || s.paneSizeM == nil || !s.paneSizeM.Available() || agent.TerminalID == "" {
		return false
	}
	handle, err := s.paneSizeM.BeginHistoryRead(ctx, paneID, agent.TerminalID)
	if err != nil {
		return false
	}
	geometry := handle.Geometry()
	_ = handle.Close()
	return geometry.InProgress || !geometry.ChangedAt.IsZero() && s.historyNow().Sub(geometry.ChangedAt) < panesize.HistorySettleWindow
}
