package relay

import (
	"math/rand/v2"
	"net"
	"time"

	"github.com/technothatch/beast-relay/internal/beast"
)

// backoffBase is the deterministic exponential delay for a dial attempt
// (0-based): min doubled per attempt, capped at max.
func backoffBase(attempt int, min, max time.Duration) time.Duration {
	if min <= 0 {
		return 0
	}
	d := min
	for i := 0; i < attempt; i++ {
		if d >= max {
			return max
		}
		next := d * 2
		if next <= 0 || next > max { // overflow or cap
			return max
		}
		d = next
	}
	return d
}

// backoff adds up to 50% random jitter on top of the exponential base so a
// fleet of relays does not stampede a restarting decoder in lockstep.
func backoff(attempt int, min, max time.Duration) time.Duration {
	base := backoffBase(attempt, min, max)
	if base <= 1 {
		return base
	}
	return base + time.Duration(rand.Int64N(int64(base/2)+1))
}

// upstreamLoop keeps a connection to the Beast source alive: it dials,
// reads until the connection fails, then waits out the backoff and retries
// until the relay shuts down.
func (r *Relay) upstreamLoop() {
	defer r.wg.Done()
	attempt, dials := 0, 0
	for r.ctx.Err() == nil {
		if dials > 0 {
			r.m.UpstreamReconnect()
		}
		dials++
		conn, err := r.cfg.DialContext(r.ctx, "tcp", r.cfg.Upstream)
		if err != nil {
			wait := backoff(attempt, r.cfg.ReconnectMin, r.cfg.ReconnectMax)
			attempt++
			r.log.Warn("upstream dial failed",
				"upstream", r.cfg.Upstream, "attempt", attempt,
				"retry_in", wait, "error", err)
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		attempt = 0
		r.m.UpstreamConnect()
		r.setUpstream(conn)
		r.m.SetUpstreamConnected(true)
		r.log.Info("upstream connected", "upstream", r.cfg.Upstream)
		r.readUpstream(conn)
		r.setUpstream(nil)
		r.m.SetUpstreamConnected(false)
		if r.ctx.Err() != nil {
			return
		}
		r.log.Warn("upstream disconnected", "upstream", r.cfg.Upstream)
	}
}

// readUpstream pumps bytes from one upstream connection to every client
// until the read fails or the relay shuts down.
func (r *Relay) readUpstream(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		r.setUpstream(nil)
		r.m.SetUpstreamConnected(false)
	}()

	// Only parse frames when a filter needs them; otherwise the bytes go
	// through untouched, which is both faster and lossless by definition.
	var parser *beast.Parser
	filtering := r.cfg.Filter.Active()
	if filtering {
		parser = beast.NewParser()
	}

	buf := make([]byte, defaultReadBufferSize)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			r.m.AddUpstreamBytes(n)
			if !filtering {
				r.hub.broadcast(cloneBytes(buf[:n]))
				continue
			}
			r.forwardFiltered(parser.Feed(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

// forwardFiltered applies the accept rules to the parsed frames and
// broadcasts everything that survives. Framing noise and bytes that fail to
// parse are forwarded as-is: a relay must not lose data it cannot classify.
func (r *Relay) forwardFiltered(segments []beast.Segment) {
	for _, seg := range segments {
		if !seg.IsFrame {
			r.hub.broadcast(seg.Raw)
			continue
		}
		frame, err := beast.Parse(seg.Raw)
		if err != nil {
			r.log.Debug("forwarding undecodable bytes", "error", err)
			r.hub.broadcast(seg.Raw)
			continue
		}
		if !r.cfg.Filter.Accept(frame) {
			r.m.FrameFiltered()
			continue
		}
		r.m.FrameForwarded()
		r.hub.broadcast(seg.Raw)
	}
}

// cloneBytes copies a read buffer so the writer goroutines never race the
// next Read.
func cloneBytes(b []byte) []byte {
	return append([]byte(nil), b...)
}
