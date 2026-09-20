package cluster

import (
	"errors"
	"sync"
)

var (
	ErrInvalidSession = errors.New("invalid session reference")
	ErrEdgeNotFound   = errors.New("edge is not registered")
	ErrSessionBound   = errors.New("session id is already bound")
	ErrStaleSession   = errors.New("stale session generation")
	ErrInvalidState   = errors.New("invalid session lifecycle transition")
)

type entry struct {
	transition sync.Mutex
	ref        SessionRef
	edge       EdgeID
	state      SessionState
	announced  bool
}

type SessionSnapshot struct {
	Ref       SessionRef
	Edge      EdgeID
	State     SessionState
	Announced bool
}

// Registry is the authoritative location and lifecycle index for live sessions.
// No transport or connection pointer is stored here.
type Registry struct {
	mu       sync.RWMutex
	edges    map[EdgeID]uint64
	sessions map[uint32]*entry
}

func NewRegistry() *Registry {
	return &Registry{edges: make(map[EdgeID]uint64), sessions: make(map[uint32]*entry)}
}

// RegisterEdge records an edge instance. generation is process-local today and
// reserves the stale-instance check needed by the future network protocol.
func (r *Registry) RegisterEdge(id EdgeID, generation uint64) error {
	if id == "" || generation == 0 {
		return ErrEdgeNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.edges[id]; ok && current != generation {
		return ErrSessionBound
	}
	r.edges[id] = generation
	return nil
}

func (r *Registry) Bind(ref SessionRef, edge EdgeID) error {
	if !ref.Valid() {
		return ErrInvalidSession
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.edges[edge]; !ok {
		return ErrEdgeNotFound
	}
	if old := r.sessions[ref.SessionID]; old != nil {
		if old.ref == ref && old.edge == edge {
			return nil
		}
		return ErrSessionBound
	}
	r.sessions[ref.SessionID] = &entry{ref: ref, edge: edge, state: StateSyncing}
	return nil
}

func (r *Registry) Snapshot(ref SessionRef) (SessionSnapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.sessions[ref.SessionID]
	if e == nil || e.ref != ref {
		return SessionSnapshot{}, false
	}
	return snapshot(e), true
}

func (r *Registry) Ref(sessionID uint32) (SessionRef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.sessions[sessionID]
	if e == nil {
		return SessionRef{}, false
	}
	return e.ref, true
}

func (r *Registry) Active(ref SessionRef) bool {
	s, ok := r.Snapshot(ref)
	return ok && s.State == StateActive
}

func (r *Registry) transition(ref SessionRef) (*entry, error) {
	r.mu.RLock()
	e := r.sessions[ref.SessionID]
	r.mu.RUnlock()
	if e == nil || e.ref != ref {
		return nil, ErrStaleSession
	}
	e.transition.Lock()
	return e, nil
}

func (r *Registry) CommitActive(ref SessionRef) error {
	return r.CommitActiveAndAnnounce(ref, nil)
}

// CommitActiveAndAnnounce 将激活、加入广播和关闭串行化；回调不得关闭本会话。
func (r *Registry) CommitActiveAndAnnounce(ref SessionRef, announce func()) error {
	e, err := r.transition(ref)
	if err != nil {
		return err
	}
	defer e.transition.Unlock()
	if err := r.commitActive(ref); err != nil {
		return err
	}
	if announce != nil {
		announce()
		r.mu.Lock()
		e.announced = true
		r.mu.Unlock()
	}
	return nil
}

func (r *Registry) commitActive(ref SessionRef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.sessions[ref.SessionID]
	if e == nil || e.ref != ref {
		return ErrStaleSession
	}
	if e.state != StateSyncing {
		return ErrInvalidState
	}
	e.state = StateActive
	return nil
}

func (r *Registry) BeginClose(ref SessionRef) (SessionSnapshot, error) {
	e0, err := r.transition(ref)
	if err != nil {
		return SessionSnapshot{}, err
	}
	defer e0.transition.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.sessions[ref.SessionID]
	if e == nil || e.ref != ref {
		return SessionSnapshot{}, ErrStaleSession
	}
	if e.state == StateClosing || e.state == StateClosed {
		s := snapshot(e)
		s.Announced = false
		return s, nil
	}
	if e.state != StateSyncing && e.state != StateActive {
		return SessionSnapshot{}, ErrInvalidState
	}
	e.state = StateClosing
	return snapshot(e), nil
}

func (r *Registry) Unbind(ref SessionRef) error {
	e, err := r.transition(ref)
	if err != nil {
		return err
	}
	defer e.transition.Unlock()
	return r.unbind(ref)
}

func (r *Registry) unbind(ref SessionRef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.sessions[ref.SessionID]
	if e == nil || e.ref != ref {
		return ErrStaleSession
	}
	e.state = StateClosed
	delete(r.sessions, ref.SessionID)
	return nil
}

func (r *Registry) RemoveEdge(id EdgeID, generation uint64) []SessionRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.edges[id]; !ok || current != generation {
		return nil
	}
	delete(r.edges, id)
	var removed []SessionRef
	for sid, e := range r.sessions {
		if e.edge == id {
			removed = append(removed, e.ref)
			delete(r.sessions, sid)
		}
	}
	return removed
}

func (r *Registry) Sessions() []SessionSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SessionSnapshot, 0, len(r.sessions))
	for _, e := range r.sessions {
		out = append(out, snapshot(e))
	}
	return out
}

func snapshot(e *entry) SessionSnapshot {
	return SessionSnapshot{Ref: e.ref, Edge: e.edge, State: e.state, Announced: e.announced}
}

// CloseAndUnbind 将同一 generation 的清理串行化，回调只执行一次。
func (r *Registry) CloseAndUnbind(ref SessionRef, cleanup func(SessionSnapshot)) error {
	e, err := r.transition(ref)
	if err != nil {
		return err
	}
	defer e.transition.Unlock()
	r.mu.Lock()
	if r.sessions[ref.SessionID] != e {
		r.mu.Unlock()
		return ErrStaleSession
	}
	snap := snapshot(e)
	if e.state == StateClosing {
		snap.Announced = false
	}
	e.state = StateClosing
	r.mu.Unlock()
	cleanup(snap)
	return r.unbind(ref)
}
