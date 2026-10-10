package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape performs one request against the handler and returns the body.
func scrape(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerExposesCounters(t *testing.T) {
	m := New()
	m.SetClients(3)
	m.ClientAccepted()
	m.ClientAccepted()
	m.ClientRejected()
	m.SetUpstreamConnected(true)
	m.UpstreamConnect()
	m.UpstreamReconnect()
	m.AddUpstreamBytes(1000)
	m.AddClientBytes(900)
	m.AddClientDroppedBytes(42)
	m.FrameForwarded()
	m.FrameFiltered()

	rec := scrape(t, Handler(m, nil), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	body := rec.Body.String()
	want := []struct {
		line string
	}{
		{"beast_relay_clients 3"},
		{"beast_relay_clients_accepted_total 2"},
		{"beast_relay_clients_rejected_total 1"},
		{"beast_relay_upstream_connected 1"},
		{"beast_relay_upstream_connects_total 1"},
		{"beast_relay_upstream_reconnects_total 1"},
		{"beast_relay_upstream_bytes_total 1000"},
		{"beast_relay_client_bytes_total 900"},
		{"beast_relay_client_dropped_bytes_total 42"},
		{"beast_relay_frames_forwarded_total 1"},
		{"beast_relay_frames_filtered_total 1"},
	}
	for _, w := range want {
		if !strings.Contains(body, w.line+"\n") {
			t.Errorf("metrics output missing %q", w.line)
		}
	}
}

func TestHandlerHealthz(t *testing.T) {
	m := New()
	h := Handler(m, func() Status { return Status{Upstream: true, Clients: 2} })
	rec := scrape(t, h, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"ok\n", "upstream: connected\n", "clients: 2\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("healthz body missing %q: %q", want, body)
		}
	}
}

func TestHandlerHealthzReportsDisconnected(t *testing.T) {
	m := New()
	h := Handler(m, func() Status { return Status{Upstream: false} })
	body := scrape(t, h, "/healthz").Body.String()
	if !strings.Contains(body, "upstream: disconnected\n") {
		t.Errorf("healthz body = %q, want upstream: disconnected", body)
	}
	if !strings.Contains(body, "clients: 0\n") {
		t.Errorf("healthz body = %q, want clients: 0", body)
	}
}

func TestHandlerHealthzWithoutStatus(t *testing.T) {
	m := New()
	rec := scrape(t, Handler(m, nil), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); body != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}
}

func TestNewRegistriesAreIndependent(t *testing.T) {
	a, b := New(), New()
	a.AddClientDroppedBytes(10)
	if body := scrape(t, Handler(b, nil), "/metrics").Body.String(); strings.Contains(body, " 10\n") {
		t.Errorf("second registry leaked counters:\n%s", body)
	}
	if a.Registry() == b.Registry() {
		t.Error("registries are shared")
	}
}
