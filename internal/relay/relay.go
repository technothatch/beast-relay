// Package relay fans one upstream Beast stream out to many downstream
// clients.
//
// The data path is deliberately boring: an upstream loop dials the source
// with exponential backoff, reads bytes, and publishes them to a hub of
// clients, each with a bounded queue written by its own goroutine. With no
// filter configured the bytes are forwarded verbatim; with a filter the
// stream is split into frames just far enough to read message type, DF and
// ICAO, and rejected frames are removed while everything else still passes
// through byte-for-byte.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/technothatch/beast-relay/internal/filter"
	"github.com/technothatch/beast-relay/internal/metrics"
)

// Config configures a Relay. The zero value is not usable: Upstream and
// Listen must be set; unset numeric fields fall back to the defaults below.
type Config struct {
	// Upstream is the Beast source, "host:30005" for dump1090/readsb.
	Upstream string
	// Listen is the address for downstream clients, e.g. ":30005".
	Listen string
	// MaxClients refuses connections beyond this count; 0 means unlimited.
	MaxClients int
	// IdleTimeout disconnects a client that still has queued data but has
	// not accepted a write for this long; it is also the per-write
	// deadline. 0 disables both.
	IdleTimeout time.Duration
	// ClientBuffer is the per-client queue size in bytes (default 1 MiB).
	ClientBuffer int
	// ReconnectMin and ReconnectMax bound the exponential dial backoff
	// (defaults 500ms and 30s).
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	// Filter decides which frames are forwarded; nil or inactive means
	// every byte is forwarded untouched.
	Filter *filter.Filter
	// Metrics records counters; a private registry is created when nil.
	Metrics *metrics.Metrics
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// DialContext overrides the upstream dialer (tests inject failures).
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
}

// Defaults applied by New when numeric fields are left at zero.
const (
	defaultClientBuffer   = 1 << 20
	defaultReconnectMin   = 500 * time.Millisecond
	defaultReconnectMax   = 30 * time.Second
	acceptRetryDelay      = 50 * time.Millisecond
	defaultReadBufferSize = 64 * 1024
)

// Relay is a running fan-out proxy: Start launches the accept, upstream and
// idle-reaper goroutines, Close stops them and drops every client.
type Relay struct {
	cfg Config
	log *slog.Logger
	m   *metrics.Metrics
	hub *hub

	mu     sync.Mutex
	ln     net.Listener
	upConn net.Conn

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	nextID atomic.Uint64
}

// New validates the config and builds a Relay; nothing is dialled or bound
// until Start.
func New(cfg Config) (*Relay, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.withDefaults()
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Relay{
		cfg: cfg,
		log: log.With("component", "relay"),
		m:   cfg.Metrics,
		hub: newHub(),
	}, nil
}

// validate rejects unusable configuration with a message naming the flag.
func (c Config) validate() error {
	if c.Upstream == "" {
		return errors.New("relay: upstream address is required")
	}
	if _, _, err := net.SplitHostPort(c.Upstream); err != nil {
		return fmt.Errorf("relay: upstream %q must be host:port: %w", c.Upstream, err)
	}
	if c.Listen == "" {
		return errors.New("relay: listen address is required")
	}
	if c.MaxClients < 0 {
		return fmt.Errorf("relay: max-clients must be >= 0, got %d", c.MaxClients)
	}
	if c.ClientBuffer < 0 {
		return fmt.Errorf("relay: client-buffer must be >= 0, got %d", c.ClientBuffer)
	}
	if c.IdleTimeout < 0 {
		return fmt.Errorf("relay: idle-timeout must be >= 0, got %s", c.IdleTimeout)
	}
	if c.ReconnectMin < 0 || c.ReconnectMax < 0 {
		return errors.New("relay: reconnect backoff must be >= 0")
	}
	if c.ReconnectMin > 0 && c.ReconnectMax > 0 && c.ReconnectMin > c.ReconnectMax {
		return fmt.Errorf("relay: reconnect-min %s exceeds reconnect-max %s", c.ReconnectMin, c.ReconnectMax)
	}
	return nil
}

// withDefaults fills unset numeric fields.
func (c *Config) withDefaults() {
	if c.ClientBuffer == 0 {
		c.ClientBuffer = defaultClientBuffer
	}
	if c.ReconnectMin == 0 {
		c.ReconnectMin = defaultReconnectMin
	}
	if c.ReconnectMax == 0 {
		c.ReconnectMax = defaultReconnectMax
	}
	if c.Metrics == nil {
		c.Metrics = metrics.New()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.DialContext == nil {
		d := &net.Dialer{}
		c.DialContext = d.DialContext
	}
}

// Start binds the client listener and launches the background loops. On
// success Addr is valid and Close must be called to shut down.
func (r *Relay) Start(parent context.Context) error {
	ln, err := net.Listen("tcp", r.cfg.Listen)
	if err != nil {
		return fmt.Errorf("relay: listen on %s: %w", r.cfg.Listen, err)
	}
	r.ctx, r.cancel = context.WithCancel(parent)
	r.mu.Lock()
	r.ln = ln
	r.mu.Unlock()

	r.wg.Add(1)
	go r.acceptLoop()
	r.wg.Add(1)
	go r.upstreamLoop()
	if r.cfg.IdleTimeout > 0 {
		r.wg.Add(1)
		go r.reaperLoop()
	}
	return nil
}

// Addr returns the bound client listener address (nil before Start).
func (r *Relay) Addr() net.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return nil
	}
	return r.ln.Addr()
}

// Status snapshots upstream connectivity and client count for /healthz.
func (r *Relay) Status() metrics.Status {
	return metrics.Status{Upstream: r.connected(), Clients: r.hub.count()}
}

// Close stops every loop, drops all clients and waits for them to exit. It
// is safe to call more than once.
func (r *Relay) Close() error {
	r.mu.Lock()
	cancel, ln := r.cancel, r.ln
	r.mu.Unlock()
	if cancel == nil {
		return nil // never started
	}
	cancel()
	if ln != nil {
		_ = ln.Close()
	}
	r.hub.closeAll()
	r.closeUpstream()
	r.wg.Wait()
	return nil
}

// acceptLoop admits downstream clients until shutdown.
func (r *Relay) acceptLoop() {
	defer r.wg.Done()
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			if r.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			r.log.Warn("accept failed", "error", err)
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(acceptRetryDelay):
			}
			continue
		}
		if r.cfg.MaxClients > 0 && r.hub.count() >= r.cfg.MaxClients {
			r.m.ClientRejected()
			r.log.Warn("rejecting client: max-clients reached",
				"remote", conn.RemoteAddr(), "max_clients", r.cfg.MaxClients)
			_ = conn.Close()
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
		}
		c := newClient(r.nextID.Add(1), conn, r.cfg.ClientBuffer, r.cfg.IdleTimeout, r.m, r.log)
		if !r.hub.add(c) {
			_ = conn.Close()
			return // hub closed: shutting down
		}
		r.m.ClientAccepted()
		r.m.SetClients(r.hub.count())
		r.log.Info("client connected", "client", c.id, "remote", conn.RemoteAddr())
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			c.run()
			r.hub.remove(c)
			r.m.SetClients(r.hub.count())
			r.log.Info("client disconnected", "client", c.id, "remote", conn.RemoteAddr())
		}()
	}
}

// reaperLoop disconnects clients that hold queued data but stop reading it.
func (r *Relay) reaperLoop() {
	defer r.wg.Done()
	interval := r.cfg.IdleTimeout / 2
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case now := <-t.C:
			for _, c := range r.hub.list() {
				chunks, last := c.pending()
				if stalled(chunks, last, now, r.cfg.IdleTimeout) {
					r.log.Warn("disconnecting stalled client",
						"client", c.id, "remote", c.conn.RemoteAddr(),
						"queued_chunks", chunks, "idle", r.cfg.IdleTimeout)
					c.close()
				}
			}
		}
	}
}

// connected reports whether an upstream read loop is currently running.
func (r *Relay) connected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.upConn != nil
}

// setUpstream records the active upstream connection (nil while down).
func (r *Relay) setUpstream(conn net.Conn) {
	r.mu.Lock()
	r.upConn = conn
	r.mu.Unlock()
}

// closeUpstream tears down the active upstream connection, unblocking its
// read loop during shutdown.
func (r *Relay) closeUpstream() {
	r.mu.Lock()
	conn := r.upConn
	r.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}
