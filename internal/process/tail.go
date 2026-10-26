package process

import "sync"

// Tail retains the last bytes written while continuing to drain pipes, so late
// failure messages survive output larger than its capacity. Interactive stdout
// and stderr may call Write concurrently.
type Tail struct {
	mu        sync.Mutex
	limit     int
	buffer    []byte
	truncated bool
}

func NewTail(limit int) *Tail {
	return &Tail{limit: limit}
}

func (tail *Tail) Write(data []byte) (int, error) {
	tail.mu.Lock()
	defer tail.mu.Unlock()
	accepted := len(data)
	if len(data) >= tail.limit {
		tail.truncated = tail.truncated || len(data) > tail.limit || len(tail.buffer) > 0
		tail.buffer = append(tail.buffer[:0], data[len(data)-tail.limit:]...)
		return accepted, nil
	}
	if drop := len(tail.buffer) + len(data) - tail.limit; drop > 0 {
		tail.truncated = true
		tail.buffer = append(tail.buffer[:0], tail.buffer[drop:]...)
	}
	tail.buffer = append(tail.buffer, data...)
	return accepted, nil
}

// Bytes returns a copy of the retained suffix and whether earlier bytes were dropped.
func (tail *Tail) Bytes() ([]byte, bool) {
	tail.mu.Lock()
	defer tail.mu.Unlock()
	return append([]byte(nil), tail.buffer...), tail.truncated
}
