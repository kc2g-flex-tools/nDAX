package pw

import (
	"io/fs"
	"sync"
)

// ring is a byte ring buffer connecting the Go side of a stream with the
// PipeWire process callback. Writes never block: when the buffer would
// overflow, the stale contents are discarded so latency stays bounded.
type ring struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	rpos   int
	length int
	closed bool
}

func newRing(capacity int) *ring {
	r := &ring{buf: make([]byte, capacity)}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, fs.ErrClosed
	}
	if len(p) > len(r.buf) {
		p = p[len(p)-len(r.buf):]
	}
	if r.length+len(p) > len(r.buf) {
		// Overflow: nobody is consuming fast enough. Drop what's
		// buffered rather than the new data, so a consumer that
		// resumes hears current audio, not stale audio.
		r.rpos = 0
		r.length = 0
	}
	wpos := (r.rpos + r.length) % len(r.buf)
	n := copy(r.buf[wpos:], p)
	copy(r.buf, p[n:])
	r.length += len(p)
	r.cond.Broadcast()
	return len(p), nil
}

// readLocked copies up to len(p) buffered bytes into p.
func (r *ring) readLocked(p []byte) int {
	want := len(p)
	if want > r.length {
		want = r.length
	}
	n := copy(p[:want], r.buf[r.rpos:])
	copy(p[n:want], r.buf)
	r.rpos = (r.rpos + want) % len(r.buf)
	r.length -= want
	return want
}

// ReadZero fills p with buffered data, zero-padding any shortfall. It never
// blocks; it is called from the PipeWire process callback.
func (r *ring) ReadZero(p []byte) {
	r.mu.Lock()
	n := 0
	if !r.closed {
		n = r.readLocked(p)
	}
	r.mu.Unlock()
	clear(p[n:])
}

// ReadBlock blocks until at least one byte is available (or the ring is
// closed) and reads up to len(p) bytes.
func (r *ring) ReadBlock(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.length == 0 && !r.closed {
		r.cond.Wait()
	}
	if r.closed {
		return 0, fs.ErrClosed
	}
	return r.readLocked(p), nil
}

func (r *ring) Close() {
	r.mu.Lock()
	r.closed = true
	r.cond.Broadcast()
	r.mu.Unlock()
}
