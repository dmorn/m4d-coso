package session

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/dmorn/m4dtimes/sdk/llm"
)

// Recorder writes events for a single user to an append-only JSONL file.
// Safe for concurrent use.
type Recorder struct {
	userID   int64
	file     *os.File
	mu       sync.Mutex
	lastID   string // parentId for the next event
}

func newRecorder(path string, userID int64) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open session file: %w", err)
	}
	r := &Recorder{userID: userID, file: f}

	// Write session manifest only if the file is new (empty).
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat session file: %w", err)
	}
	if info.Size() == 0 {
		init := sessionInitEvent(userID)
		if err := r.writeEvent(init); err != nil {
			_ = f.Close()
			return nil, err
		}
		r.lastID = init.ID
	}

	return r, nil
}

// Record appends a message event to the JSONL file.
func (r *Recorder) Record(msg llm.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e := messageEvent(msg, r.lastID)
	if err := r.writeEvent(e); err != nil {
		// Best-effort: log to stderr, never panic
		fmt.Fprintf(os.Stderr, "session recorder: write error for user %d: %v\n", r.userID, err)
		return
	}
	r.lastID = e.ID
}

// Close flushes and closes the underlying file.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}

func (r *Recorder) writeEvent(e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	b = append(b, '\n')
	_, err = r.file.Write(b)
	return err
}
