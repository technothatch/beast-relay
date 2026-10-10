package relay

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/technothatch/beast-relay/internal/beast"
	"github.com/technothatch/beast-relay/internal/filter"
	"github.com/technothatch/beast-relay/internal/metrics"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// metricValue reads one counter or gauge out of the registry.
func metricValue(t *testing.T, m *metrics.Metrics, name string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		metric := f.GetMetric()[0]
		if c := metric.GetCounter(); c != nil {
			return c.GetValue()
		}
		return metric.GetGauge().GetValue()
	}
	t.Fatalf("metric %q not found", name)
	return 0
}

// fakeUpstream is a Beast source tests write frames into. The first
// failFirst connections are accepted and immediately dropped, which is how
// the reconnect path is exercised.
type fakeUpstream struct {
	ln        net.Listener
	failFirst int64
	accepted  atomic.Int64

	mu    sync.Mutex
	conns []net.Conn
}

func newFakeUpstream(t *testing.T, failFirst int) *fakeUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream fixture: %v", err)
	}
	u := &fakeUpstream{ln: ln, failFirst: int64(failFirst)}
	go u.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		u.mu.Lock()
		defer u.mu.Unlock()
		for _, c := range u.conns {
			_ = c.Close()
		}
	})
	return u
}

func (u *fakeUpstream) serve() {
	for {
		conn, err := u.ln.Accept()
		if err != nil {
			return
		}
		n := u.accepted.Add(1)
		u.mu.Lock()
		u.conns = append(u.conns, conn)
		u.mu.Unlock()
		if n <= u.failFirst {
			_ = conn.Close()
		}
	}
}

func (u *fakeUpstream) addr() string { return u.ln.Addr().String() }

// acceptedCount returns how many connections were accepted so far.
func (u *fakeUpstream) acceptedCount() int64 { return u.accepted.Load() }

// write sends b on every accepted connection; dead ones just error out.
func (u *fakeUpstream) write(b []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range u.conns {
		_, _ = c.Write(b)
	}
}

// startRelay boots a relay against a fixture address with fast backoff.
func startRelay(t *testing.T, upstream string, cfg Config) (*Relay, *metrics.Metrics) {
	t.Helper()
	m := metrics.New()
	cfg.Upstream = upstream
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	cfg.Metrics = m
	cfg.Logger = testLogger(t)
	if cfg.ReconnectMin == 0 {
		cfg.ReconnectMin = 5 * time.Millisecond
	}
	if cfg.ReconnectMax == 0 {
		cfg.ReconnectMax = 50 * time.Millisecond
	}
	rel, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := rel.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rel.Close() })
	return rel, m
}

// dialClient connects one downstream consumer.
func dialClient(t *testing.T, rel *Relay) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", rel.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readExact reads n bytes or fails the test.
func readExact(t *testing.T, conn net.Conn, n int, timeout time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}

// fixtureFrame builds a decodable DF17 frame for a given ICAO.
func fixtureFrame(t *testing.T, icao uint32) []byte {
	t.Helper()
	payload := make([]byte, 14)
	payload[0] = 17<<3 | 5
	payload[1], payload[2], payload[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	raw, err := beast.Encode(beast.Frame{Type: beast.TypeModeSLong, MLAT: 0x010203040506, Signal: 0xe0, Payload: payload})
	if err != nil {
		t.Fatalf("encode fixture frame: %v", err)
	}
	return raw
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "missing upstream", cfg: Config{Listen: ":0"}, wantErr: true},
		{name: "upstream without port", cfg: Config{Upstream: "dump1090", Listen: ":0"}, wantErr: true},
		{name: "missing listen", cfg: Config{Upstream: "localhost:30005"}, wantErr: true},
		{name: "negative max clients", cfg: Config{Upstream: "h:1", Listen: ":0", MaxClients: -1}, wantErr: true},
		{name: "negative buffer", cfg: Config{Upstream: "h:1", Listen: ":0", ClientBuffer: -1}, wantErr: true},
		{name: "negative idle timeout", cfg: Config{Upstream: "h:1", Listen: ":0", IdleTimeout: -time.Second}, wantErr: true},
		{name: "inverted backoff", cfg: Config{Upstream: "h:1", Listen: ":0", ReconnectMin: 2 * time.Second, ReconnectMax: time.Second}, wantErr: true},
		{name: "valid applies defaults", cfg: Config{Upstream: "h:1", Listen: ":0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel, err := New(tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("New succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if rel.cfg.ClientBuffer != defaultClientBuffer {
				t.Errorf("ClientBuffer = %d, want %d", rel.cfg.ClientBuffer, defaultClientBuffer)
			}
			if rel.cfg.ReconnectMin != defaultReconnectMin || rel.cfg.ReconnectMax != defaultReconnectMax {
				t.Errorf("backoff = [%s, %s], want [%s, %s]",
					rel.cfg.ReconnectMin, rel.cfg.ReconnectMax, defaultReconnectMin, defaultReconnectMax)
			}
			if rel.cfg.DialContext == nil || rel.cfg.Metrics == nil {
				t.Error("defaults for DialContext/Metrics not applied")
			}
		})
	}
}

func TestFanOutDeliversIdenticalBytes(t *testing.T) {
	up := newFakeUpstream(t, 0)
	rel, m := startRelay(t, up.addr(), Config{})

	waitFor(t, 5*time.Second, func() bool { return rel.Status().Upstream }, "upstream connection")

	const clients = 3
	conns := make([]net.Conn, clients)
	for i := range conns {
		conns[i] = dialClient(t, rel)
	}
	waitFor(t, 5*time.Second, func() bool { return rel.Status().Clients == clients }, "clients to register")

	// Two frames plus a dump1090-style heartbeat, sent as one stream.
	f1 := fixtureFrame(t, 0x4ca123)
	f2 := fixtureFrame(t, 0xabc123)
	heartbeat := []byte{beast.Marker, beast.TypeModeAC, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	payload := append(append(append([]byte(nil), f1...), f2...), heartbeat...)
	up.write(payload)

	for i, conn := range conns {
		got := readExact(t, conn, len(payload), 5*time.Second)
		if !bytes.Equal(got, payload) {
			t.Errorf("client %d received %d bytes that do not match the upstream stream", i, len(got))
		}
	}

	waitFor(t, 5*time.Second, func() bool {
		return metricValue(t, m, metrics.NameClientBytesTotal) == float64(clients*len(payload))
	}, "client byte counter")
	if got := metricValue(t, m, metrics.NameUpstreamBytesTotal); got != float64(len(payload)) {
		t.Errorf("upstream_bytes_total = %v, want %d", got, len(payload))
	}
	if got := metricValue(t, m, metrics.NameUpstreamConnectsTotal); got != 1 {
		t.Errorf("upstream_connects_total = %v, want 1", got)
	}
	if got := metricValue(t, m, metrics.NameUpstreamReconnectsTotal); got != 0 {
		t.Errorf("upstream_reconnects_total = %v, want 0 on a first dial", got)
	}
	if !rel.Status().Upstream || rel.Status().Clients != clients {
		t.Errorf("status = %+v, want upstream connected with %d clients", rel.Status(), clients)
	}
}

func TestMaxClientsRefusesExtraConnections(t *testing.T) {
	up := newFakeUpstream(t, 0)
	rel, m := startRelay(t, up.addr(), Config{MaxClients: 1})

	dialClient(t, rel)
	waitFor(t, 5*time.Second, func() bool { return rel.Status().Clients == 1 }, "first client")

	second := dialClient(t, rel)
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if n, err := second.Read(buf); n != 0 || err == nil {
		t.Errorf("second connection read = (%d, %v), want immediate EOF", n, err)
	}

	waitFor(t, 5*time.Second, func() bool {
		return metricValue(t, m, metrics.NameClientsRejectedTotal) == 1
	}, "rejected client counter")
	if got := metricValue(t, m, metrics.NameClientsAcceptedTotal); got != 1 {
		t.Errorf("clients_accepted_total = %v, want 1", got)
	}
	if rel.Status().Clients != 1 {
		t.Errorf("clients = %d, want 1", rel.Status().Clients)
	}
}

func TestFilteringDropsOnlyRejectedFrames(t *testing.T) {
	flt, err := filter.New(filter.Config{DenyHex: "4ca123"})
	if err != nil {
		t.Fatalf("filter.New: %v", err)
	}
	up := newFakeUpstream(t, 0)
	rel, m := startRelay(t, up.addr(), Config{Filter: flt})
	waitFor(t, 5*time.Second, func() bool { return rel.Status().Upstream }, "upstream connection")

	conn := dialClient(t, rel)
	waitFor(t, 5*time.Second, func() bool { return rel.Status().Clients == 1 }, "client to register")

	kept := fixtureFrame(t, 0xabc123)
	garbage := []byte{0xde, 0xad, 0x00, 0xbe}
	dropped := fixtureFrame(t, 0x4ca123)
	stream := append(append(append([]byte(nil), kept...), garbage...), dropped...)
	up.write(stream)

	want := append(append([]byte(nil), kept...), garbage...)
	got := readExact(t, conn, len(want), 5*time.Second)
	if !bytes.Equal(got, want) {
		t.Errorf("forwarded % x, want % x", got, want)
	}
	// No trailing bytes: the denied frame must never arrive.
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	extra := make([]byte, 1)
	if n, err := conn.Read(extra); err == nil && n > 0 {
		t.Error("denied frame leaked to the client")
	}

	waitFor(t, 5*time.Second, func() bool {
		return metricValue(t, m, metrics.NameFramesFilteredTotal) == 1
	}, "filtered frame counter")
	if got := metricValue(t, m, metrics.NameFramesForwardedTotal); got != 1 {
		t.Errorf("frames_forwarded_total = %v, want 1", got)
	}
	if got := metricValue(t, m, metrics.NameUpstreamBytesTotal); got != float64(len(stream)) {
		t.Errorf("upstream_bytes_total = %v, want %d", got, len(stream))
	}
}

func TestIdleTimeoutRemovesStalledClient(t *testing.T) {
	up := newFakeUpstream(t, 0)
	rel, m := startRelay(t, up.addr(), Config{
		IdleTimeout:  400 * time.Millisecond,
		ClientBuffer: 64 << 10,
	})
	waitFor(t, 5*time.Second, func() bool { return rel.Status().Upstream }, "upstream connection")

	dialClient(t, rel) // never reads: this is the one that should stall
	healthy := dialClient(t, rel)
	healthyDone := make(chan struct{})
	go func() {
		defer close(healthyDone)
		_, _ = io.Copy(io.Discard, healthy) // keeps the healthy client draining
	}()
	waitFor(t, 5*time.Second, func() bool { return rel.Status().Clients == 2 }, "both clients")

	// Blast far more than any buffer holds so the stalled client's queue
	// fills and its writer goes quiet.
	big := make([]byte, 1<<20)
	for i := 0; i < 32; i++ {
		up.write(big)
	}

	waitFor(t, 15*time.Second, func() bool { return rel.Status().Clients == 1 }, "stalled client to be reaped")
	waitFor(t, 5*time.Second, func() bool {
		return metricValue(t, m, metrics.NameClientDroppedBytesTotal) > 0
	}, "dropped byte counter")

	// The healthy client must survive the reaper.
	time.Sleep(600 * time.Millisecond)
	if got := rel.Status().Clients; got != 1 {
		t.Errorf("clients = %d after idle period, want 1 (the healthy one)", got)
	}
	select {
	case <-healthyDone:
		t.Error("healthy client was disconnected")
	default:
	}
}
