package panesize

import (
	"context"
	"errors"
	"fmt"
)

type historyPin struct {
	terminal  Terminal
	paneID    string
	identity  terminalIdentity
	borrowers int
	retired   bool
	queued    bool
}

func (m *Manager) notifyHistoryLocked() {
	close(m.historyChanged)
	m.historyChanged = make(chan struct{})
}

func (m *Manager) retireHistoryPinLocked(state *historyGeometryState) {
	if state == nil || state.pin == nil {
		return
	}
	pin := state.pin
	state.pin = nil
	state.forceFresh = true
	pin.retired = true
	m.queueHistoryCloseLocked(pin)
	m.notifyHistoryLocked()
}

func (m *Manager) queueHistoryCloseLocked(pin *historyPin) {
	if pin.retired && pin.borrowers == 0 && !pin.queued {
		pin.queued = true
		m.historyClosing = append(m.historyClosing, pin)
	}
}

func (m *Manager) closeRetiredHistoryPins() error {
	m.mu.Lock()
	closing := m.historyClosing
	m.historyClosing = nil
	m.mu.Unlock()
	var failures []error
	for _, pin := range closing {
		if err := pin.terminal.Close(); err != nil {
			failures = append(failures, err)
		}
		m.mu.Lock()
		delete(m.historyPins, pin)
		m.notifyHistoryLocked()
		m.mu.Unlock()
	}
	return errors.Join(failures...)
}

func (m *Manager) releaseHistoryPin(pin *historyPin) error {
	m.mu.Lock()
	pin.borrowers--
	m.queueHistoryCloseLocked(pin)
	m.notifyHistoryLocked()
	m.mu.Unlock()
	return m.closeRetiredHistoryPins()
}

func (m *Manager) beginPinnedHistoryRead(ctx context.Context, paneID, terminalID string) (*HistoryRead, error) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, ErrClosed
		}
		state := m.historyGeometry[paneID]
		if state == nil {
			state = &historyGeometryState{identity: terminalIdentity{terminalID: terminalID}}
			m.historyGeometry[paneID] = state
		}
		if state.identity.terminalID != terminalID {
			currentLease, _ := m.leaseHistoryStateLocked(paneID, terminalID)
			if !currentLease {
				m.mu.Unlock()
				return nil, gone("history terminal was replaced")
			}
			m.retireHistoryPinLocked(state)
			state = &historyGeometryState{identity: terminalIdentity{terminalID: terminalID}}
			m.historyGeometry[paneID] = state
		}
		if pin := state.pin; pin != nil {
			leased, _ := m.leaseHistoryStateLocked(paneID, terminalID)
			if !leased && !historyIntervalUnsafe(state.geometry, m.now()) {
				m.retireHistoryPinLocked(state)
			} else {
				pin.borrowers++
				state.forgotten = false
				revision := state.geometry.Revision
				m.mu.Unlock()
				return m.samplePinnedHistoryRead(ctx, paneID, terminalID, state, pin, revision)
			}
		}
		if state.resolving != nil {
			wait := state.resolving
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
				continue
			}
		}
		blocked := false
		for pin := range m.historyPins {
			if pin.paneID == paneID {
				blocked = true
				break
			}
		}
		if blocked {
			m.mu.Unlock()
			_ = m.closeRetiredHistoryPins()
			return nil, ErrHistoryReadUnsafe
		}
		wait := make(chan struct{})
		state.resolving = wait
		opCtx, cancel := context.WithTimeout(ctx, m.opTimeout)
		m.historyResolving[wait] = cancel
		_, leaseRevision := m.leaseHistoryStateLocked(paneID, terminalID)
		revision := state.geometry.Revision
		m.mu.Unlock()
		identity, err := m.lookupIdentity(opCtx, paneID)
		var terminal Terminal
		if err == nil && identity.terminalID != terminalID {
			err = gone("history terminal was replaced")
		}
		if err == nil {
			terminal, err = m.openValidated(opCtx, paneID, &identity, true)
		}
		cancel()
		m.mu.Lock()
		_, currentLeaseRevision := m.leaseHistoryStateLocked(paneID, terminalID)
		obsolete := m.closed || m.historyGeometry[paneID] != state || state.resolving != wait || state.geometry.Revision != revision || currentLeaseRevision != leaseRevision || ctx.Err() != nil
		if state.resolving == wait {
			state.resolving = nil
			close(wait)
		}
		if err != nil || obsolete {
			if m.historyGeometry[paneID] == state {
				m.historyUnknownLocked(paneID, terminalID)
				state.forceFresh = true
			}
			m.mu.Unlock()
			if terminal != nil {
				_ = terminal.Close()
			}
			m.mu.Lock()
			delete(m.historyResolving, wait)
			m.notifyHistoryLocked()
			m.mu.Unlock()
			_ = m.closeRetiredHistoryPins()
			if err != nil {
				return nil, err
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrHistoryReadUnsafe
		}
		if state.identity != identity {
			state = m.historyStateLocked(paneID, identity)
			revision = state.geometry.Revision
		}
		state.identity = identity
		state.forceFresh = false
		state.forgotten = false
		state.geometry.PaneID, state.geometry.TerminalID = paneID, terminalID
		pin := &historyPin{terminal: terminal, paneID: paneID, identity: identity, borrowers: 1}
		state.pin = pin
		m.historyPins[pin] = true
		delete(m.historyResolving, wait)
		m.notifyHistoryLocked()
		m.mu.Unlock()
		return m.samplePinnedHistoryRead(ctx, paneID, terminalID, state, pin, revision)
	}
}

func (m *Manager) samplePinnedHistoryRead(ctx context.Context, paneID, terminalID string, state *historyGeometryState, pin *historyPin, revision uint64) (*HistoryRead, error) {
	readCtx, cancel := context.WithCancel(ctx)
	read := &HistoryRead{manager: m, ctx: readCtx, terminal: pin.terminal, pin: pin, state: state, cancel: cancel}
	read.mu.Lock()
	m.mu.Lock()
	m.historyReads[read] = cancel
	if m.closed {
		cancel()
	}
	m.mu.Unlock()
	current, err := pin.terminal.Size()
	m.mu.Lock()
	unsafe := m.closed || pin.retired || m.historyGeometry[paneID] != state || state.geometry.Revision != revision || state.geometry.InProgress
	if err != nil || current.Columns == 0 || current.Rows == 0 {
		if m.historyGeometry[paneID] == state {
			m.historyUnknownLocked(paneID, terminalID)
		}
		geometry := state.geometry
		m.mu.Unlock()
		failure := fmt.Errorf("%w: %v", ErrSizeUnavailable, err)
		read.completeLocked(geometry, failure)
		read.mu.Unlock()
		return nil, failure
	}
	if !unsafe {
		m.observeHistoryLocked(state, current)
	}
	geometry := state.geometry
	geometry.Leased, geometry.LeaseRevision = m.leaseHistoryStateLocked(paneID, terminalID)
	started := m.now()
	m.mu.Unlock()
	read.start, read.started, read.unsafe = geometry, started, unsafe
	read.stop = context.AfterFunc(readCtx, func() { _ = read.Close() })
	read.mu.Unlock()
	if readCtx.Err() != nil {
		_ = read.Close()
		return nil, readCtx.Err()
	}
	return read, nil
}
