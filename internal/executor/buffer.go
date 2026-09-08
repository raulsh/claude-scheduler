package executor

import "sync"

// boundedBuffer collects up to limit bytes and silently discards the rest.
// It is used for stderr, where only the first part is diagnostic and an
// unbounded buffer would be a memory risk on a misbehaving run.
type boundedBuffer struct {
	mu       sync.Mutex
	buf      []byte
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if room := b.limit - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
			b.overflow = true
		}
	} else {
		b.overflow = true
	}
	// Report the full length: the writer is not at fault for the cap.
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.overflow {
		return string(b.buf) + "\n... (output truncated)"
	}
	return string(b.buf)
}
