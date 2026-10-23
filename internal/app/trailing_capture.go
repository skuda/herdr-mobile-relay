package app

import (
	"context"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
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

func (p *pendingHistoryCapture) expired(now time.Time) bool {
	return p.attempts >= trailingCaptureAttempts || now.Sub(p.queued) >= trailingCaptureLifetime
}

// enqueueHistoryTransition records accepted departures without coordinator lookups or pane reads.
func (s *Server) enqueueHistoryTransition(event coordinator.StatusTransition) {
	if !coordinator.IsAttentionStatus(event.OldStatus) || coordinator.IsAttentionStatus(event.NewStatus) {
		return
	}
	s.historyPendingMu.Lock()
	defer s.historyPendingMu.Unlock()
	if s.historyPendingClosed {
		return
	}
	if event.Revision <= s.historyStatusRevision[event.PaneID] {
		return
	}
	if active, ok := s.historyPendingActive[event.PaneID]; ok && (event.Generation < active.generation || event.Generation == active.generation && active.terminalID != event.TerminalID) {
		return
	}
	s.historyStatusRevision[event.PaneID] = event.Revision
	s.historyRequestSequence++
	if existing := s.historyPending[event.PaneID]; existing != nil && existing.identity.terminalID == event.TerminalID {
		existing.sequence = s.historyRequestSequence
		existing.revision = event.Revision
		return
	}
	s.historyPending[event.PaneID] = &pendingHistoryCapture{identity: historyIdentity{terminalID: event.TerminalID, generation: event.Generation},
		sequence: s.historyRequestSequence, revision: event.Revision, queued: s.historyNow()}
}

func (s *Server) captureHistoryTick(now time.Time) {
	agents := s.state.Snapshot()
	s.syncHistoryPanes()
	candidates := make(map[string]bool)
	for _, agent := range agents {
		if isClaudeLike(agent.Agent) && coordinator.IsAttentionStatus(agent.Status) {
			candidates[agent.PaneID] = true
		}
	}
	var expired []string
	s.historyPendingMu.Lock()
	for paneID, pending := range s.historyPending {
		if pending.expired(now) {
			delete(s.historyPending, paneID)
			expired = append(expired, paneID)
			continue
		}
		candidates[paneID] = true
	}
	s.historyPendingMu.Unlock()
	for _, paneID := range expired {
		s.logger.Warn("trailing pane capture expired", "pane_id", paneID)
	}
	for paneID := range candidates {
		s.scheduleHistoryCaptureAt(context.Background(), paneID, now)
	}
}

func (s *Server) pendingReadStartedLocked(paneID string, identity historyIdentity) (uint64, bool) {
	pending := s.historyPending[paneID]
	if pending == nil || pending.identity != identity {
		return 0, false
	}
	if pending.expired(s.historyNow()) {
		delete(s.historyPending, paneID)
		return 0, true
	}
	pending.attempts++
	return pending.sequence, false
}

func (s *Server) acknowledgeHistoryLocked(token historyReadToken) {
	if pending := s.historyPending[token.paneID]; pending != nil && pending.identity == token.identity && pending.sequence == token.pendingSequence {
		delete(s.historyPending, token.paneID)
		s.historyAcknowledgements++
	}
}

func (s *Server) historyCaptureDeferred(paneID string) bool {
	agent, ok := s.state.Agent(paneID)
	if !ok || s.paneSizeM == nil || !s.paneSizeM.Available() || agent.TerminalID == "" {
		return false
	}
	return s.paneSizeM.HistoryReadUnsafe(paneID, agent.TerminalID)
}
