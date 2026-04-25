// Package memory provides an in-memory Store used for unit tests and for
// capsuled when no persistent backend is wired up (e.g. during bring-up).
package memory

import (
	"context"
	"sort"
	"sync"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/store"
	"google.golang.org/protobuf/proto"
)

type memStore struct {
	workloads *workloadStore
	volumes   *volumeStore
	osState   *osStateStore
}

// New returns a fresh in-memory Store.
func New() store.Store {
	return &memStore{
		workloads: &workloadStore{items: map[string]*capsulev1.Workload{}},
		volumes:   &volumeStore{items: map[string]*capsulev1.Volume{}},
		osState:   &osStateStore{},
	}
}

func (m *memStore) Workloads() store.WorkloadStore { return m.workloads }
func (m *memStore) Volumes() store.VolumeStore     { return m.volumes }
func (m *memStore) OSState() store.OSStateStore    { return m.osState }
func (m *memStore) Close() error                   { return nil }

type osStateStore struct {
	mu    sync.RWMutex
	state *store.OSState
}

func (s *osStateStore) Get(_ context.Context) (*store.OSState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state == nil {
		return nil, store.ErrNotFound
	}
	cp := *s.state
	return &cp, nil
}

func (s *osStateStore) Put(_ context.Context, st *store.OSState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *st
	s.state = &cp
	return nil
}

type volumeStore struct {
	mu    sync.RWMutex
	items map[string]*capsulev1.Volume
}

func (s *volumeStore) Put(_ context.Context, v *capsulev1.Volume) error {
	if v.GetName() == "" {
		return store.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[v.GetName()] = proto.Clone(v).(*capsulev1.Volume)
	return nil
}

func (s *volumeStore) Get(_ context.Context, name string) (*capsulev1.Volume, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.items[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return proto.Clone(v).(*capsulev1.Volume), nil
}

func (s *volumeStore) List(_ context.Context) ([]*capsulev1.Volume, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*capsulev1.Volume, 0, len(s.items))
	for _, v := range s.items {
		out = append(out, proto.Clone(v).(*capsulev1.Volume))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *volumeStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, name)
	return nil
}

type workloadStore struct {
	mu    sync.RWMutex
	items map[string]*capsulev1.Workload
}

func (s *workloadStore) Put(_ context.Context, w *capsulev1.Workload) error {
	if w.GetName() == "" {
		return store.ErrNotFound // callers expect a meaningful error; core validates first
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[w.Name] = proto.Clone(w).(*capsulev1.Workload)
	return nil
}

func (s *workloadStore) Get(_ context.Context, name string) (*capsulev1.Workload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.items[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return proto.Clone(w).(*capsulev1.Workload), nil
}

func (s *workloadStore) List(_ context.Context) ([]*capsulev1.Workload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*capsulev1.Workload, 0, len(s.items))
	for _, w := range s.items {
		out = append(out, proto.Clone(w).(*capsulev1.Workload))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *workloadStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, name)
	return nil
}
