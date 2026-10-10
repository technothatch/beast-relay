package relay

import "sync"

// hub tracks the connected downstream clients and fans frames out to all
// of them. Broadcast holds only the read lock: enqueueing onto a client is
// non-blocking by construction (bounded queue, drop-oldest), so one stuck
// consumer can never stall the others.
type hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	closed  bool
}

func newHub() *hub {
	return &hub{clients: make(map[*client]struct{})}
}

// add registers a client, unless the hub is already closed (the accept
// loop raced a shutdown); the caller closes the connection when add fails.
func (h *hub) add(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.clients[c] = struct{}{}
	return true
}

// remove unregisters a client.
func (h *hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
}

// count returns the number of connected clients.
func (h *hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// list snapshots the current clients for the idle reaper.
func (h *hub) list() []*client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, c)
	}
	return out
}

// broadcast queues b on every client without blocking.
func (h *hub) broadcast(b []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		c.enqueue(b)
	}
}

// closeAll closes every client and refuses further adds.
func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		c.close()
	}
	clear(h.clients)
}
