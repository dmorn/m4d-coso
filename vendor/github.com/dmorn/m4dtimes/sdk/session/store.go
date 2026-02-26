package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/dmorn/m4dtimes/sdk/llm"
)

// Store manages one Recorder per user, lazily creating JSONL files under dir.
// File layout: <dir>/<userID>.jsonl
// Safe for concurrent use.
type Store struct {
	dir       string
	mu        sync.Mutex
	recorders map[int64]*Recorder
}

// NewStore creates a Store that writes session files to dir.
// The directory is created if it does not exist.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	return &Store{
		dir:       dir,
		recorders: make(map[int64]*Recorder),
	}, nil
}

// Record appends msg to the session file for userID.
// The Recorder (and its JSONL file) is created on first call for each user.
func (s *Store) Record(userID int64, msg llm.Message) {
	r, err := s.recorderFor(userID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session store: open recorder for user %d: %v\n", userID, err)
		return
	}
	r.Record(msg)
}

// Close flushes and closes all open recorders.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recorders {
		_ = r.Close()
	}
	s.recorders = make(map[int64]*Recorder)
}

func (s *Store) recorderFor(userID int64) (*Recorder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recorders[userID]; ok {
		return r, nil
	}
	path := filepath.Join(s.dir, fmt.Sprintf("%d.jsonl", userID))
	r, err := newRecorder(path, userID)
	if err != nil {
		return nil, err
	}
	s.recorders[userID] = r
	return r, nil
}
