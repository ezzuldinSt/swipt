package automation

import (
	"slices"
	"strconv"
	"sync"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/device"
)

const keptSnapshots = 64

type snapshotStore struct {
	access sync.Mutex
	views  map[string]*device.View
	order  []string
	next   int
}

type snapshot struct {
	ID string `json:"id"`
	*device.View
}

func newSnapshotStore() *snapshotStore {
	return &snapshotStore{views: make(map[string]*device.View)}
}

func (s *snapshotStore) keep(view *device.View) snapshot {
	s.access.Lock()
	defer s.access.Unlock()
	s.next++
	id := strconv.Itoa(s.next)
	s.views[id] = view
	s.order = append(s.order, id)
	if len(s.order) > keptSnapshots {
		delete(s.views, s.order[0])
		s.order = slices.Delete(s.order, 0, 1)
	}
	return snapshot{ID: id, View: view}
}

func (s *snapshotStore) get(id string) (*device.View, error) {
	s.access.Lock()
	defer s.access.Unlock()
	view, found := s.views[id]
	if !found {
		return nil, E.Extend(ErrInvalid, "the snapshot was dropped to keep the newest ", keptSnapshots, "; read the screen again")
	}
	index := slices.Index(s.order, id)
	s.order = append(slices.Delete(s.order, index, index+1), id)
	return view, nil
}
