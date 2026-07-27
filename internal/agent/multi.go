package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Multi presents several agents as one board. Discovery runs them in parallel;
// every other operation is routed back to the adapter whose name the session
// carries.
type Multi struct {
	adapters []Adapter
}

func NewMulti(adapters ...Adapter) *Multi {
	return &Multi{adapters: adapters}
}

func (m *Multi) Name() string {
	names := make([]string, 0, len(m.adapters))
	for _, adapter := range m.adapters {
		names = append(names, adapter.Name())
	}
	sort.Strings(names)
	return joinNames(names)
}

// Discover keeps whatever the healthy agents returned. One agent being absent
// or broken is reported alongside the sessions rather than blanking the board,
// because the common case is a machine where only some of them are installed.
func (m *Multi) Discover(ctx context.Context, limit int) ([]Session, error) {
	var (
		mutex    sync.Mutex
		sessions []Session
		failures []error
		wait     sync.WaitGroup
	)
	for _, adapter := range m.adapters {
		wait.Add(1)
		go func(adapter Adapter) {
			defer wait.Done()
			found, err := adapter.Discover(ctx, limit)
			mutex.Lock()
			defer mutex.Unlock()
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", adapter.Name(), err))
				return
			}
			sessions = append(sessions, found...)
		}(adapter)
	}
	wait.Wait()

	if len(failures) == len(m.adapters) {
		return nil, errors.Join(failures...)
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].RecencyAt.After(sessions[j].RecencyAt)
	})
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return sessions, errors.Join(failures...)
}

func (m *Multi) Preview(
	ctx context.Context,
	session Session,
	limit int,
) (Transcript, error) {
	adapter, err := m.adapterFor(session)
	if err != nil {
		return Transcript{}, err
	}
	return adapter.Preview(ctx, session, limit)
}

func (m *Multi) ResumeCommand(session Session) (string, []string) {
	adapter, err := m.adapterFor(session)
	if err != nil {
		return "", nil
	}
	return adapter.ResumeCommand(session)
}

func (m *Multi) Archive(ctx context.Context, session Session) error {
	adapter, err := m.adapterFor(session)
	if err != nil {
		return err
	}
	return adapter.Archive(ctx, session)
}

func (m *Multi) adapterFor(session Session) (Adapter, error) {
	for _, adapter := range m.adapters {
		if adapter.Name() == session.Agent {
			return adapter, nil
		}
	}
	return nil, fmt.Errorf("no adapter for agent %q", session.Agent)
}

func joinNames(names []string) string {
	result := ""
	for i, name := range names {
		if i > 0 {
			result += "+"
		}
		result += name
	}
	return result
}
