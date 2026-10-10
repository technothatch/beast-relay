package relay

import "sync"

// queue is a byte-bounded FIFO of raw stream chunks for one slow client.
//
// When a push would exceed maxBytes, the oldest chunks are evicted until it
// fits — drop-oldest, never blocking the producer. That keeps the fan-out
// bus moving no matter how slow a consumer is; the evicted bytes are
// accounted and surfaced as client_dropped_bytes_total. A single chunk
// larger than maxBytes is still queued (empty queue), so one oversized
// frame can never be silently discarded.
type queue struct {
	mu sync.Mutex

	items [][]byte
	head  int
	bytes int

	maxBytes     int
	droppedBytes uint64
}

// newQueue returns a queue holding at most maxBytes of chunks.
func newQueue(maxBytes int) *queue {
	return &queue{maxBytes: maxBytes}
}

// push appends b, evicting oldest chunks as needed, and returns the number
// of bytes dropped to make room.
func (q *queue) push(b []byte) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	dropped := 0
	for q.lenLocked() > 0 && q.bytes+len(b) > q.maxBytes {
		oldest := q.popLocked()
		q.droppedBytes += uint64(len(oldest))
		dropped += len(oldest)
	}
	q.compactLocked()
	q.items = append(q.items, b)
	q.bytes += len(b)
	return dropped
}

// pop removes and returns the oldest chunk.
func (q *queue) pop() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.lenLocked() == 0 {
		return nil, false
	}
	return q.popLocked(), true
}

// stats reports the queued chunk count, queued bytes and cumulative
// dropped bytes.
func (q *queue) stats() (count, bytes int, dropped uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lenLocked(), q.bytes, q.droppedBytes
}

// lenLocked is the number of queued chunks.
func (q *queue) lenLocked() int { return len(q.items) - q.head }

// popLocked evicts the oldest chunk and updates the byte accounting.
func (q *queue) popLocked() []byte {
	b := q.items[q.head]
	q.items[q.head] = nil
	q.head++
	q.bytes -= len(b)
	if q.head == len(q.items) {
		q.items = q.items[:0]
		q.head = 0
	}
	return b
}

// compactLocked slides consumed chunks out of the ring so push never grows
// the backing array beyond the peak queue depth.
func (q *queue) compactLocked() {
	if q.head == 0 || len(q.items) != cap(q.items) {
		return
	}
	copy(q.items, q.items[q.head:])
	q.items = q.items[:len(q.items)-q.head]
	q.head = 0
}
