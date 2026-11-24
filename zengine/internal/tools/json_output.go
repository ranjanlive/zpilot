package tools

import (
	"bytes"
	"sync"
)

const maxJSONOutput = 8 << 20

// jsonOutput keeps draining stdout after overflow so a child cannot deadlock
// on a full pipe. The incomplete document is never parsed or sent to the model.
type jsonOutput struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	overflow bool
}

func (b *jsonOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := maxJSONOutput - b.buf.Len()
	if n > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.buf.Write(p)
	return n, nil
}
func (b *jsonOutput) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}
func (b *jsonOutput) Overflowed() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.overflow }
