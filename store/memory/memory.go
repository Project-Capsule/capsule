// Package memory provides an in-memory Store used for unit tests and for
// capsuled when no persistent backend is wired up (e.g. during bring-up).
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/store"
	"google.golang.org/protobuf/proto"
)

type memStore struct {
	workloads *workloadStore
	volumes   *volumeStore
	osState   *osStateStore
	identity  *identityStore
	authKeys  *authKeyStore
}

// New returns a fresh in-memory Store.
func New() store.Store {
	return &memStore{
		workloads: &workloadStore{items: map[string]*capsulev1.Workload{}},
		volumes:   &volumeStore{items: map[string]*capsulev1.Volume{}},
		osState:   &osStateStore{},
		identity:  &identityStore{},
		authKeys:  &authKeyStore{items: map[string]*store.AuthorizedKey{}},
	}
}

func (m *memStore) Workloads() store.WorkloadStore           { return m.workloads }
func (m *memStore) Volumes() store.VolumeStore               { return m.volumes }
func (m *memStore) OSState() store.OSStateStore              { return m.osState }
func (m *memStore) Identity() store.IdentityStore            { return m.identity }
func (m *memStore) AuthorizedKeys() store.AuthorizedKeyStore { return m.authKeys }
func (m *memStore) Close() error                             { return nil }

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

type identityStore struct {
	mu sync.RWMutex
	id *store.CapsuleIdentity
}

func (s *identityStore) Get(_ context.Context) (*store.CapsuleIdentity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.id == nil {
		return nil, store.ErrNotFound
	}
	cp := *s.id
	return &cp, nil
}

func (s *identityStore) Put(_ context.Context, id *store.CapsuleIdentity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *id
	if cp.CreatedAtUnix == 0 {
		cp.CreatedAtUnix = time.Now().Unix()
	}
	s.id = &cp
	return nil
}

type authKeyStore struct {
	mu    sync.RWMutex
	items map[string]*store.AuthorizedKey
}

func (s *authKeyStore) Add(_ context.Context, k *store.AuthorizedKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[k.Kid]; ok {
		return store.ErrConflict
	}
	cp := *k
	if cp.AddedAtUnix == 0 {
		cp.AddedAtUnix = time.Now().Unix()
	}
	cp.Pubkey = append([]byte(nil), k.Pubkey...)
	s.items[cp.Kid] = &cp
	return nil
}

func (s *authKeyStore) Get(_ context.Context, kid string) (*store.AuthorizedKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.items[kid]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *k
	cp.Pubkey = append([]byte(nil), k.Pubkey...)
	return &cp, nil
}

func (s *authKeyStore) List(_ context.Context) ([]*store.AuthorizedKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*store.AuthorizedKey, 0, len(s.items))
	for _, k := range s.items {
		cp := *k
		cp.Pubkey = append([]byte(nil), k.Pubkey...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AddedAtUnix != out[j].AddedAtUnix {
			return out[i].AddedAtUnix < out[j].AddedAtUnix
		}
		return out[i].Kid < out[j].Kid
	})
	return out, nil
}

func (s *authKeyStore) Delete(_ context.Context, kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, kid)
	return nil
}

func (s *authKeyStore) Count(_ context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items), nil
}

func (s *authKeyStore) DeleteAll(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = map[string]*store.AuthorizedKey{}
	return nil
}
