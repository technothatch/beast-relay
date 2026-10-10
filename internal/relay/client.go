package relay

import (
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/technothatch/beast-relay/internal/metrics"
)

// client is one downstream Beast consumer with its own bounded queue and
// writer goroutine.
//
// enqueue never blocks: it pushes onto the queue (evicting the oldest
// chunks when full) and pokes the writer. The writer drains the queue and
// writes to the connection with a deadline, so a peer that stops reading
// costs the relay its queue, never the fan-out bus.
type client struct {
	id   uint64
	conn net.Conn
	q    *queue
	m    *metrics.Metrics
	log  *slog.Logger
	idle time.Duration

	// signal wakes the writer; capacity one, poked without blocking.
	signal chan struct{}
	// done is closed by close, ending the writer goroutine.
	done      chan struct{}
	closeOnce sync.Once

	// lastWrite is the Unix nanos of the last successful write, used by
	// the idle reaper to spot stalled clients.
	lastWrite atomic.Int64
}

// newClient adopts an accepted connection.
func newClient(id uint64, conn net.Conn, buffer int, idle time.Duration, m *metrics.Metrics, log *slog.Logger) *client {
	c := &client{
		id:     id,
		conn:   conn,
		q:      newQueue(buffer),
		m:      m,
		log:    log,
		idle:   idle,
		signal: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	c.lastWrite.Store(time.Now().UnixNano())
	return c
}

// enqueue queues bytes for the client without ever blocking the caller.
func (c *client) enqueue(b []byte) {
	if dropped := c.q.push(b); dropped > 0 {
		c.m.AddClientDroppedBytes(dropped)
		c.log.Debug("slow client, dropped oldest queued bytes",
			"client", c.id, "remote", c.conn.RemoteAddr(), "dropped_bytes", dropped)
	}
	select {
	case c.signal <- struct{}{}:
	default: // writer is already scheduled to wake up
	}
}

// run drains the queue to the connection until writing fails or the client
// is closed.
func (c *client) run() {
	for {
		select {
		case <-c.done:
			return
		case <-c.signal:
		}
		for {
			b, ok := c.q.pop()
			if !ok {
				break
			}
			if c.idle > 0 {
				_ = c.conn.SetWriteDeadline(time.Now().Add(c.idle))
			}
			n, err := c.conn.Write(b)
			if n > 0 {
				c.m.AddClientBytes(n)
				c.lastWrite.Store(time.Now().UnixNano())
			}
			if err != nil {
				c.log.Debug("client write failed", "client", c.id, "error", err)
				return
			}
		}
	}
}

// close shuts the client down exactly once; it is safe from any goroutine
// and unblocks a writer parked in Write.
func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// pending returns the queued chunk count and the last write time, for the
// idle reaper.
func (c *client) pending() (chunks int, lastWrite time.Time) {
	count, _, _ := c.q.stats()
	return count, time.Unix(0, c.lastWrite.Load())
}

// stalled reports whether a client still has queued data but has not
// accepted a write within timeout — the reaper's disconnect rule. A quiet
// client with an empty queue is never stalled, however long the feed is
// silent.
func stalled(chunks int, lastWrite, now time.Time, timeout time.Duration) bool {
	if timeout <= 0 || chunks == 0 {
		return false
	}
	return now.Sub(lastWrite) > timeout
}
