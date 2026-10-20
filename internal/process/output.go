package process

import (
	"bytes"
	"sync"
)

// Output bounds retained child-process output while continuing to drain pipes.
// Interactive stdout and stderr may call Write concurrently.
type Output struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

func (output *Output) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	accepted := len(data)
	remaining := (1 << 20) - output.buffer.Len()
	if len(data) > remaining {
		output.truncated = true
		data = data[:remaining]
	}
	_, _ = output.buffer.Write(data)
	return accepted, nil
}

func (output *Output) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	text := output.buffer.String()
	if output.truncated {
		text += "\n[swarm: output truncated at 1 MiB]"
	}
	return text
}
