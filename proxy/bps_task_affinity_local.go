package proxy

import (
	"container/list"
	"context"
	"errors"
	"sync"

	"github.com/codex2api/database"
)

// Task affinity for heuristic seeds (conversation-prefix inferred sessions).
// Such a seed can be shared by unrelated conversations with the same opening,
// so with persist_heuristic_affinity off only client task IDs are bound in the
// database and heuristic seeds keep their soft preference in this bounded
// in-process LRU (no cross-replica stickiness). It mirrors the database's
// revision semantics.

type bpsTaskAffinityStore interface {
	ReadBPSTaskAffinity(ctx context.Context, key string) (database.BPSTaskAffinity, error)
	UpdateBPSTaskAffinity(ctx context.Context, key string, revision, accountID int64) (database.BPSTaskAffinity, error)
}

const bpsLocalTaskAffinityLimit = 4096

type bpsLocalTaskAffinity struct {
	key    string
	record database.BPSTaskAffinity
}

type bpsLocalTaskAffinityStore struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
}

var bpsLocalTaskAffinities bpsLocalTaskAffinityStore

func (s *bpsLocalTaskAffinityStore) ReadBPSTaskAffinity(_ context.Context, key string) (database.BPSTaskAffinity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.entries[key]; entry != nil {
		s.order.MoveToBack(entry)
		return entry.Value.(bpsLocalTaskAffinity).record, nil
	}
	return database.BPSTaskAffinity{}, nil
}

// UpdateBPSTaskAffinity binds key to accountID when revision still matches;
// the revision advances only when the account changes.
func (s *bpsLocalTaskAffinityStore) UpdateBPSTaskAffinity(_ context.Context, key string, revision, accountID int64) (database.BPSTaskAffinity, error) {
	if key == "" || revision < 0 || accountID <= 0 {
		return database.BPSTaskAffinity{}, errors.New("invalid BPS task affinity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]*list.Element)
	}
	entry := s.entries[key]
	if entry == nil {
		record := database.BPSTaskAffinity{AccountID: accountID, Revision: 1}
		s.entries[key] = s.order.PushBack(bpsLocalTaskAffinity{key: key, record: record})
		for len(s.entries) > bpsLocalTaskAffinityLimit {
			oldest := s.order.Front()
			delete(s.entries, oldest.Value.(bpsLocalTaskAffinity).key)
			s.order.Remove(oldest)
		}
		return record, nil
	}
	current := entry.Value.(bpsLocalTaskAffinity)
	s.order.MoveToBack(entry)
	if current.record.Revision != revision {
		return current.record, nil
	}
	if current.record.AccountID != accountID {
		current.record = database.BPSTaskAffinity{AccountID: accountID, Revision: current.record.Revision + 1}
		entry.Value = current
	}
	return current.record, nil
}

// bpsTaskAffinityStoreFor picks where a seed's affinity lives: the database,
// unless the seed is heuristic and persist_heuristic_affinity is off. It
// returns nil when the database is needed but absent.
func (h *Handler) bpsTaskAffinityStoreFor(state *inferredBPSSession) (bpsTaskAffinityStore, string) {
	if state.diagnostic.Heuristic && !currentBPSConfig().PersistsHeuristicAffinity() {
		return &bpsLocalTaskAffinities, "local"
	}
	if h.db == nil {
		return nil, ""
	}
	return h.db, "database"
}
