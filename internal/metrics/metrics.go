// Package metrics owns the Prometheus registry of the relay and the HTTP
// handler that exposes it.
//
// Every metric is a plain counter or gauge with no labels, so the time
// series index stays fixed no matter how many clients connect or how many
// aircraft are in the air.
package metrics

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric names, exported so dashboards and tests reference them without
// repeating string literals. clientDroppedBytesTotal keeps the plain
// "client_dropped_bytes_total" suffix Prometheus alert rules key on.
const (
	NameClients                 = "beast_relay_clients"
	NameClientsAcceptedTotal    = "beast_relay_clients_accepted_total"
	NameClientsRejectedTotal    = "beast_relay_clients_rejected_total"
	NameUpstreamConnected       = "beast_relay_upstream_connected"
	NameUpstreamConnectsTotal   = "beast_relay_upstream_connects_total"
	NameUpstreamReconnectsTotal = "beast_relay_upstream_reconnects_total"
	NameUpstreamBytesTotal      = "beast_relay_upstream_bytes_total"
	NameClientBytesTotal        = "beast_relay_client_bytes_total"
	NameClientDroppedBytesTotal = "beast_relay_client_dropped_bytes_total"
	NameFramesForwardedTotal    = "beast_relay_frames_forwarded_total"
	NameFramesFilteredTotal     = "beast_relay_frames_filtered_total"
)

// Status is the snapshot /healthz reports.
type Status struct {
	// Upstream is true while a Beast source is connected.
	Upstream bool
	// Clients is the number of connected downstream clients.
	Clients int
}

// Metrics owns one private registry of relay collectors. All methods are
// safe for concurrent use: the relay updates while Prometheus scrapes.
type Metrics struct {
	registry *prometheus.Registry

	clients            prometheus.Gauge
	clientsAccepted    prometheus.Counter
	clientsRejected    prometheus.Counter
	upstreamConnected  prometheus.Gauge
	upstreamConnects   prometheus.Counter
	upstreamReconnects prometheus.Counter
	upstreamBytes      prometheus.Counter
	clientBytes        prometheus.Counter
	clientDroppedBytes prometheus.Counter
	framesForwarded    prometheus.Counter
	framesFiltered     prometheus.Counter
}

// New builds a Metrics with every collector registered.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		clients: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: NameClients,
			Help: "Downstream Beast clients currently connected.",
		}),
		clientsAccepted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameClientsAcceptedTotal,
			Help: "Downstream client connections accepted since start.",
		}),
		clientsRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameClientsRejectedTotal,
			Help: "Downstream connections refused because --max-clients was reached.",
		}),
		upstreamConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: NameUpstreamConnected,
			Help: "1 while the upstream Beast source is connected, 0 otherwise.",
		}),
		upstreamConnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameUpstreamConnectsTotal,
			Help: "Successful connections to the upstream Beast source.",
		}),
		upstreamReconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameUpstreamReconnectsTotal,
			Help: "Reconnect attempts to the upstream source: dials after the first.",
		}),
		upstreamBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameUpstreamBytesTotal,
			Help: "Bytes read from the upstream Beast source.",
		}),
		clientBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameClientBytesTotal,
			Help: "Bytes written to downstream clients.",
		}),
		clientDroppedBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameClientDroppedBytesTotal,
			Help: "Bytes dropped from slow clients' queues (drop-oldest, never stall the bus).",
		}),
		framesForwarded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameFramesForwardedTotal,
			Help: "Beast frames forwarded to clients; counted only when a filter is active.",
		}),
		framesFiltered: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameFramesFilteredTotal,
			Help: "Beast frames dropped by --filter-min-rssi/--allow-hex/--deny-hex.",
		}),
	}
	collectors := []prometheus.Collector{
		m.clients, m.clientsAccepted, m.clientsRejected,
		m.upstreamConnected, m.upstreamConnects, m.upstreamReconnects,
		m.upstreamBytes, m.clientBytes, m.clientDroppedBytes,
		m.framesForwarded, m.framesFiltered,
	}
	for _, c := range collectors {
		if err := m.registry.Register(c); err != nil {
			panic(fmt.Sprintf("metrics: register collector: %v", err))
		}
	}
	return m
}

// Registry returns the private registry serving every relay metric.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// SetClients records the current downstream client count.
func (m *Metrics) SetClients(n int) { m.clients.Set(float64(n)) }

// ClientAccepted counts an accepted downstream connection.
func (m *Metrics) ClientAccepted() { m.clientsAccepted.Inc() }

// ClientRejected counts a connection refused by --max-clients.
func (m *Metrics) ClientRejected() { m.clientsRejected.Inc() }

// SetUpstreamConnected flips the upstream connection gauge.
func (m *Metrics) SetUpstreamConnected(v bool) {
	if v {
		m.upstreamConnected.Set(1)
		return
	}
	m.upstreamConnected.Set(0)
}

// UpstreamConnect counts a successful upstream dial.
func (m *Metrics) UpstreamConnect() { m.upstreamConnects.Inc() }

// UpstreamReconnect counts a reconnect attempt (any dial after the first).
func (m *Metrics) UpstreamReconnect() { m.upstreamReconnects.Inc() }

// AddUpstreamBytes counts bytes read from the upstream source.
func (m *Metrics) AddUpstreamBytes(n int) { m.upstreamBytes.Add(float64(n)) }

// AddClientBytes counts bytes written to downstream clients.
func (m *Metrics) AddClientBytes(n int) { m.clientBytes.Add(float64(n)) }

// AddClientDroppedBytes counts bytes evicted from a slow client's queue.
func (m *Metrics) AddClientDroppedBytes(n int) { m.clientDroppedBytes.Add(float64(n)) }

// FrameForwarded counts a frame passed to clients (filter mode only).
func (m *Metrics) FrameForwarded() { m.framesForwarded.Inc() }

// FrameFiltered counts a frame rejected by an active filter rule.
func (m *Metrics) FrameFiltered() { m.framesFiltered.Inc() }

// Handler serves /metrics plus a liveness probe at /healthz. status may be
// nil, in which case /healthz only reports "ok".
func Handler(m *Metrics, status func() Status) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		var b strings.Builder
		b.WriteString("ok\n")
		if status != nil {
			s := status()
			state := "disconnected"
			if s.Upstream {
				state = "connected"
			}
			fmt.Fprintf(&b, "upstream: %s\n", state)
			fmt.Fprintf(&b, "clients: %d\n", s.Clients)
		}
		_, _ = w.Write([]byte(b.String()))
	})
	return mux
}
