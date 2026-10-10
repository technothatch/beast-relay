package relay

import (
	"testing"
	"time"

	"github.com/technothatch/beast-relay/internal/metrics"
)

func TestBackoffBase(t *testing.T) {
	cases := []struct {
		name    string
		attempt int
		min     time.Duration
		max     time.Duration
		want    time.Duration
	}{
		{"first attempt uses the floor", 0, 500 * time.Millisecond, 30 * time.Second, 500 * time.Millisecond},
		{"doubles", 1, 500 * time.Millisecond, 30 * time.Second, time.Second},
		{"doubles again", 4, 500 * time.Millisecond, 30 * time.Second, 8 * time.Second},
		{"caps at maximum", 8, 500 * time.Millisecond, 30 * time.Second, 30 * time.Second},
		{"stays capped", 64, 500 * time.Millisecond, 30 * time.Second, 30 * time.Second},
		{"tiny window caps early", 1, 10 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond},
		{"cap equals floor", 5, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond},
		{"zero floor", 3, 0, time.Second, 0},
		{"no overflow at absurd attempts", 1000, time.Millisecond, time.Minute, time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := backoffBase(tc.attempt, tc.min, tc.max)
			if got != tc.want {
				t.Errorf("backoffBase(%d, %s, %s) = %s, want %s",
					tc.attempt, tc.min, tc.max, got, tc.want)
			}
		})
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	for attempt := 0; attempt <= 6; attempt++ {
		base := backoffBase(attempt, 100*time.Millisecond, 10*time.Second)
		for i := 0; i < 200; i++ {
			got := backoff(attempt, 100*time.Millisecond, 10*time.Second)
			if got < base {
				t.Fatalf("attempt %d: backoff %s below base %s", attempt, got, base)
			}
			if got > base+base/2 {
				t.Fatalf("attempt %d: backoff %s exceeds base %s + 50%%", attempt, got, base)
			}
			if got > 10*time.Second+5*time.Second {
				t.Fatalf("attempt %d: backoff %s above the configured ceiling", attempt, got)
			}
		}
	}
}

func TestUpstreamReconnectsAfterFailures(t *testing.T) {
	// The first two connections are accepted and dropped immediately, so
	// the relay must walk the backoff before the third one sticks.
	up := newFakeUpstream(t, 2)
	rel, m := startRelay(t, up.addr(), Config{
		ReconnectMin: 5 * time.Millisecond,
		ReconnectMax: 20 * time.Millisecond,
	})

	conn := dialClient(t, rel)
	waitFor(t, 10*time.Second, func() bool { return rel.Status().Clients == 1 }, "client to register")
	waitFor(t, 10*time.Second, func() bool { return up.acceptedCount() >= 3 }, "third upstream attempt")

	waitFor(t, 5*time.Second, func() bool {
		return metricValue(t, m, metrics.NameUpstreamConnectsTotal) >= 1
	}, "successful upstream connect")
	reconnects := metricValue(t, m, metrics.NameUpstreamReconnectsTotal)
	if reconnects < 2 {
		t.Errorf("upstream_reconnects_total = %v, want at least 2 (one per failed dial)", reconnects)
	}
	waitFor(t, 5*time.Second, func() bool { return rel.Status().Upstream }, "upstream status after reconnect")

	// The reconnected stream must still reach the downstream client.
	frame := fixtureFrame(t, 0x4ca123)
	up.write(frame)
	got := readExact(t, conn, len(frame), 5*time.Second)
	if string(got) != string(frame) {
		t.Error("frame after reconnect was not forwarded verbatim")
	}
}

func TestUpstreamDisconnectFlagsStatus(t *testing.T) {
	up := newFakeUpstream(t, 0)
	rel, m := startRelay(t, up.addr(), Config{
		ReconnectMin: 5 * time.Millisecond,
		ReconnectMax: 20 * time.Millisecond,
	})
	waitFor(t, 5*time.Second, func() bool { return rel.Status().Upstream }, "upstream connected")
	if got := metricValue(t, m, metrics.NameUpstreamConnected); got != 1 {
		t.Errorf("upstream_connected gauge = %v, want 1", got)
	}

	// Kill the live connection: the relay must dial again (counted as a
	// reconnect) and come back up.
	up.dropAll()
	waitFor(t, 10*time.Second, func() bool {
		return rel.Status().Upstream && metricValue(t, m, metrics.NameUpstreamReconnectsTotal) >= 1
	}, "upstream to reconnect after the drop")
	if got := metricValue(t, m, metrics.NameUpstreamConnectsTotal); got < 2 {
		t.Errorf("upstream_connects_total = %v, want >= 2", got)
	}
}

// dropAll closes every accepted connection, simulating a decoder restart.
func (u *fakeUpstream) dropAll() {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range u.conns {
		_ = c.Close()
	}
	u.conns = nil
}
