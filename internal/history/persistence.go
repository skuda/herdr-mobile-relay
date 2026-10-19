package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"
)

type persistenceJob struct {
	paneID   string
	instance *PaneState
	state    PaneState
	version  uint64
}

func (m *Manager) FlushDue(now time.Time) error { return m.flush(now, false) }

func (m *Manager) flush(now time.Time, force bool) error {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.Lock()
	var jobs []persistenceJob
	for paneID, state := range m.states {
		if !force && (!state.dirty || now.Before(state.saveDue)) {
			continue
		}
		snapshot := *state
		snapshot.History, snapshot.Footer = slices.Clone(state.History), slices.Clone(state.Footer)
		jobs = append(jobs, persistenceJob{paneID: paneID, instance: state, state: snapshot, version: state.mutationVersion})
	}
	m.mu.Unlock()
	var failures []error
	for _, job := range jobs {
		if err := m.saveState(job, now); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) persistenceError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var failures []error
	for _, paneID := range slices.Sorted(maps.Keys(m.states)) {
		if err := m.states[paneID].saveError; err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) saveState(job persistenceJob, now time.Time) error {
	data, err := json.Marshal(&job.state)
	if err == nil {
		err = m.writeFile(m.stateFile(job.paneID)+".tmp", data, 0o600)
	}
	if err == nil && m.beforeRename != nil {
		m.beforeRename()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.states[job.paneID]
	if state != job.instance {
		if removeErr := os.Remove(m.stateFile(job.paneID) + ".tmp"); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("remove obsolete pane history temporary file: %w", removeErr)
		}
		return nil
	}
	// Keep the instance check and rename under the mutex used by Discard.
	if err == nil {
		err = os.Rename(m.stateFile(job.paneID)+".tmp", m.stateFile(job.paneID))
	}
	if err != nil {
		state.dirty = true
		state.saveDue = m.now().Add(time.Second)
		state.saveError = fmt.Errorf("save pane history %q: %w", job.paneID, err)
		return state.saveError
	}
	state.saveError = nil
	m.lastSave[job.paneID] = now
	if state.mutationVersion == job.version {
		state.dirty = false
		state.saveDue = time.Time{}
	} else {
		state.saveDue = now.Add(SaveInterval)
	}
	return nil
}

func (m *Manager) RunPersistence(ctx context.Context) {
	m.RunPersistenceWithReport(ctx, nil)
}

func (m *Manager) RunPersistenceWithReport(ctx context.Context, report func(error)) {
	lastFailure := ""
	for {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		var due time.Time
		for _, state := range m.states {
			if state.dirty && (due.IsZero() || state.saveDue.Before(due)) {
				due = state.saveDue
			}
		}
		m.mu.Unlock()
		var timer *time.Timer
		var tick <-chan time.Time
		if !due.IsZero() {
			timer = time.NewTimer(max(time.Duration(0), due.Sub(m.now())))
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-m.wake:
			if timer != nil {
				timer.Stop()
			}
		case <-tick:
			_ = m.FlushDue(m.now())
			err := m.persistenceError()
			failure := ""
			if err != nil {
				failure = err.Error()
			}
			if failure != lastFailure {
				lastFailure = failure
				if report != nil {
					report(err)
				}
			}
		}
	}
}
