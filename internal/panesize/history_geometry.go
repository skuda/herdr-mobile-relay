package panesize

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const HistorySettleWindow = 3 * time.Second

var (
	ErrHistoryReadUnsafe = errors.New("pane history read crossed an unsafe interval")
	ErrHistoryReadClosed = errors.New("pane history read is closed")
)

// HistoryGeometry describes sampled PTY rows and columns, not the emulator grid.
type HistoryGeometry struct {
	PaneID        string
	TerminalID    string
	Columns       int
	Rows          int
	Epoch         uint64
	Revision      uint64
	ChangedAt     time.Time
	Known         bool
	InProgress    bool
	Leased        bool
	LeaseRevision uint64
}

type historyGeometryState struct {
	identity  terminalIdentity
	geometry  HistoryGeometry
	baseline  bool
	forgotten bool
}

// HistoryRead holds a validated descriptor until Finish, Close, or cancellation.
type HistoryRead struct {
	mu       sync.Mutex
	manager  *Manager
	ctx      context.Context
	terminal Terminal
	state    *historyGeometryState
	start    HistoryGeometry
	started  time.Time
	unsafe   bool
	stop     func() bool
	done     bool
	result   HistoryGeometry
	err      error
}

// LeaseHistoryState includes pending restore work without checking a reader's lease.
func (m *Manager) LeaseHistoryState(paneID, terminalID string) (bool, uint64) {
	if m == nil || terminalID == "" {
		return false, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leaseHistoryStateLocked(paneID, terminalID)
}

func (m *Manager) leaseHistoryStateLocked(paneID, terminalID string) (bool, uint64) {
	var revision uint64
	if history := m.historyGeometry[paneID]; history != nil && history.identity.terminalID == terminalID {
		revision = history.geometry.LeaseRevision
	}
	state := m.panes[paneID]
	on := state != nil && state.resolved && terminalID != "" && state.identity.terminalID == terminalID &&
		(len(state.leases) > 0 || state.cleanup != nil)
	return on, revision
}

func (m *Manager) nextHistoryRevisionLocked() uint64 {
	m.historyRevision++
	return m.historyRevision
}

func (m *Manager) historyStateLocked(paneID string, identity terminalIdentity) *historyGeometryState {
	state := m.historyGeometry[paneID]
	if state != nil && state.identity == identity {
		state.forgotten = false
		return state
	}
	state = &historyGeometryState{
		identity: identity,
		geometry: HistoryGeometry{PaneID: paneID, TerminalID: identity.terminalID, Revision: m.nextHistoryRevisionLocked()},
	}
	m.historyGeometry[paneID] = state
	return state
}

func (m *Manager) observeHistoryLocked(state *historyGeometryState, current Winsize) {
	geometry := &state.geometry
	if geometry.InProgress {
		return
	}
	columns, rows := int(current.Columns), int(current.Rows)
	if !state.baseline {
		geometry.Epoch = m.nextHistoryRevisionLocked()
		state.baseline = true
	} else if geometry.Columns != columns || geometry.Rows != rows {
		geometry.Epoch = m.nextHistoryRevisionLocked()
		geometry.ChangedAt = m.now()
	}
	if !geometry.Known || geometry.Columns != columns || geometry.Rows != rows {
		geometry.Revision = m.nextHistoryRevisionLocked()
	}
	geometry.Columns, geometry.Rows = columns, rows
	geometry.Known = columns > 0 && rows > 0
}

func (m *Manager) historyUnknownLocked(paneID, terminalID string) {
	state := m.historyGeometry[paneID]
	if state == nil || state.identity.terminalID != terminalID {
		return
	}
	state.geometry.Known = false
	state.geometry.Revision = m.nextHistoryRevisionLocked()
}

func (m *Manager) historyUnknown(paneID, terminalID string) {
	m.mu.Lock()
	m.historyUnknownLocked(paneID, terminalID)
	m.mu.Unlock()
}

func (m *Manager) historyLeaseChangedLocked(paneID string, identity terminalIdentity) {
	state := m.historyStateLocked(paneID, identity)
	state.geometry.LeaseRevision = m.nextHistoryRevisionLocked()
}

func (m *Manager) setHistorySize(paneID string, identity terminalIdentity, terminal Terminal, current Winsize, want size) error {
	m.mu.Lock()
	state := m.historyStateLocked(paneID, identity)
	m.observeHistoryLocked(state, current)
	next, changed := resized(current, want)
	if !changed {
		m.mu.Unlock()
		return nil
	}
	previousSize := size{columns: state.geometry.Columns, rows: state.geometry.Rows}
	state.geometry.InProgress = true
	state.geometry.Revision = m.nextHistoryRevisionLocked()
	m.mu.Unlock()

	err := terminal.SetSize(next)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.historyGeometry[paneID] != state {
		return err
	}
	state.geometry.InProgress = false
	state.geometry.Revision = m.nextHistoryRevisionLocked()
	if err != nil {
		state.geometry.Known = false
		return err
	}
	state.geometry.Columns, state.geometry.Rows = previousSize.columns, previousSize.rows
	m.observeHistoryLocked(state, next)
	return nil
}

func (m *Manager) BeginHistoryRead(ctx context.Context, paneID, terminalID string) (*HistoryRead, error) {
	if !m.Available() {
		return nil, ErrUnavailable
	}
	if paneID == "" || terminalID == "" {
		return nil, ErrIdentityUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	var identity terminalIdentity
	cached := m.historyGeometry[paneID]
	if cached != nil && cached.identity.terminalID == terminalID {
		identity = cached.identity
	} else {
		cached = nil
	}
	m.mu.Unlock()

	opCtx, cancel := context.WithTimeout(ctx, m.opTimeout)
	defer cancel()
	capture := identity.terminalID == ""
	var err error
	if capture {
		identity, err = m.lookupIdentity(opCtx, paneID)
		if err != nil {
			m.historyUnknown(paneID, terminalID)
			return nil, err
		}
	}
	if identity.terminalID != terminalID {
		m.historyUnknown(paneID, terminalID)
		return nil, gone("history terminal was replaced")
	}
	terminal, err := m.openValidated(opCtx, paneID, &identity, capture)
	if err != nil && cached != nil && errors.Is(err, ErrTerminalGone) {
		m.mu.Lock()
		current := m.historyGeometry[paneID] == cached
		if current {
			delete(m.historyGeometry, paneID)
		}
		m.mu.Unlock()
		if current {
			identity, err = m.lookupIdentity(opCtx, paneID)
			if err == nil && identity.terminalID != terminalID {
				err = gone("history terminal was replaced")
			}
			if err == nil {
				terminal, err = m.openValidated(opCtx, paneID, &identity, true)
			}
		}
	}
	if err != nil {
		m.historyUnknown(paneID, terminalID)
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = terminal.Close()
		return nil, ErrClosed
	}
	if current := m.historyGeometry[paneID]; current != nil && current != cached && current.identity != identity {
		m.mu.Unlock()
		_ = terminal.Close()
		return nil, ErrHistoryReadUnsafe
	}
	state := m.historyStateLocked(paneID, identity)
	revision := state.geometry.Revision
	m.mu.Unlock()
	current, err := terminal.Size()
	if err != nil || current.Columns == 0 || current.Rows == 0 {
		_ = terminal.Close()
		m.historyUnknown(paneID, terminalID)
		return nil, fmt.Errorf("%w: %v", ErrSizeUnavailable, err)
	}
	m.mu.Lock()
	unsafe := m.historyGeometry[paneID] != state || state.geometry.Revision != revision || state.geometry.InProgress
	if !unsafe {
		m.observeHistoryLocked(state, current)
	}
	geometry := state.geometry
	geometry.Leased, geometry.LeaseRevision = m.leaseHistoryStateLocked(paneID, terminalID)
	started := m.now()
	m.mu.Unlock()
	read := &HistoryRead{manager: m, ctx: ctx, terminal: terminal, state: state, start: geometry, started: started, unsafe: unsafe}
	read.mu.Lock()
	read.stop = context.AfterFunc(ctx, func() { _ = read.Close() })
	read.mu.Unlock()
	if err := ctx.Err(); err != nil {
		_ = read.Close()
		return nil, err
	}
	return read, nil
}

func (r *HistoryRead) Geometry() HistoryGeometry {
	return r.start
}

func historySettling(geometry HistoryGeometry, at time.Time) bool {
	return !geometry.ChangedAt.IsZero() && at.Sub(geometry.ChangedAt) < HistorySettleWindow
}

// Finish samples the held descriptor and freezes the interval's validation result.
func (r *HistoryRead) Finish() (HistoryGeometry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return r.result, r.err
	}
	if err := r.ctx.Err(); err != nil {
		r.completeLocked(r.start, err)
		return r.result, r.err
	}
	m := r.manager
	m.mu.Lock()
	revision := r.state.geometry.Revision
	m.mu.Unlock()
	current, err := r.terminal.Size()
	m.mu.Lock()
	geometry := r.state.geometry
	unsafe := r.unsafe || m.closed || m.historyGeometry[r.start.PaneID] != r.state ||
		revision != geometry.Revision || geometry.InProgress
	if err != nil || current.Columns == 0 || current.Rows == 0 {
		if m.historyGeometry[r.start.PaneID] == r.state {
			m.historyUnknownLocked(r.start.PaneID, r.start.TerminalID)
		}
		err = fmt.Errorf("%w: %v", ErrSizeUnavailable, err)
	} else if !unsafe {
		m.observeHistoryLocked(r.state, current)
	}
	geometry = r.state.geometry
	geometry.Leased, geometry.LeaseRevision = m.leaseHistoryStateLocked(r.start.PaneID, r.start.TerminalID)
	unsafe = unsafe || !r.start.Known || r.start.InProgress || !geometry.Known ||
		geometry.Revision != r.start.Revision || geometry.Epoch != r.start.Epoch ||
		geometry.Leased != r.start.Leased || geometry.LeaseRevision != r.start.LeaseRevision ||
		historySettling(r.start, r.started) || historySettling(geometry, m.now())
	m.mu.Unlock()
	if r.ctx.Err() != nil {
		err = r.ctx.Err()
	} else if err == nil && unsafe {
		err = ErrHistoryReadUnsafe
	}
	r.completeLocked(geometry, err)
	return r.result, r.err
}

func (r *HistoryRead) completeLocked(geometry HistoryGeometry, err error) {
	if r.stop != nil {
		r.stop()
	}
	closeErr := r.terminal.Close()
	r.result, r.err, r.done = geometry, errors.Join(err, closeErr), true
}

func (r *HistoryRead) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil
	}
	err := r.ctx.Err()
	if err == nil {
		err = ErrHistoryReadClosed
	}
	r.completeLocked(r.start, err)
	return nil
}

func (m *Manager) HistoryReadUnsafe(paneID, terminalID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.historyGeometry[paneID]
	return state != nil && terminalID != "" && state.identity.terminalID == terminalID &&
		(state.geometry.InProgress || historySettling(state.geometry, m.now()))
}

func (m *Manager) ForgetHistoryPane(paneID, terminalID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.historyGeometry[paneID]; state != nil && terminalID != "" && state.identity.terminalID == terminalID {
		if state.geometry.InProgress || historySettling(state.geometry, m.now()) {
			state.forgotten = true
			state.geometry.Revision = m.nextHistoryRevisionLocked()
			return
		}
		delete(m.historyGeometry, paneID)
	}
}
