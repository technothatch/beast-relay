package relay

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/technothatch/beast-relay/internal/metrics"
)

// startClient wires a client to one end of an in-memory pipe and runs its
// writer; the test reads from the other end.
func startClient(t *testing.T, buffer int) (*client, net.Conn, <-chan struct{}) {
	t.Helper()
	local, remote := net.Pipe()
	c := newClient(1, local, buffer, 0, metrics.New(), testLogger(t))
	done := make(chan struct{})
	go func() {
		c.run()
		close(done)
	}()
	t.Cleanup(func() {
		c.close()
		_ = remote.Close()
	})
	return c, remote, done
}

func TestClientDeliversBytesInOrder(t *testing.T) {
	const chunks, size = 5, 100
	c, remote, _ := startClient(t, 1<<20)

	var want []byte
	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, chunks*size)
		if _, err := io.ReadFull(remote, buf); err != nil {
			received <- nil
			return
		}
		received <- buf
	}()
	for i := 0; i < chunks; i++ {
		fill := byte(i + 1)
		c.enqueue(chunk(size, fill))
		want = append(want, chunk(size, fill)...)
	}
	select {
	case got := <-received:
		if got == nil {
			t.Fatal("reader failed")
		}
		if string(got) != string(want) {
			t.Errorf("received bytes out of order or wrong")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for bytes")
	}
}

func TestClientBackpressureDropsOldest(t *testing.T) {
	const (
		chunks = 10
		size   = 60
		buffer = 200
	)
	c, remote, _ := startClient(t, buffer)
	m := c.m
	total := chunks * size

	// Nobody reads from remote: the writer takes the first chunk, blocks
	// in Write, and the queue fills until the drop-oldest policy kicks in.
	for i := 0; i < chunks; i++ {
		c.enqueue(chunk(size, byte(i+1)))
	}
	waitFor(t, 5*time.Second, func() bool {
		return metricValue(t, m, metrics.NameClientDroppedBytesTotal) > 0
	}, "dropped byte counter")

	dropped := int(metricValue(t, m, metrics.NameClientDroppedBytesTotal))
	if dropped%size != 0 {
		t.Fatalf("dropped = %d, not a multiple of the %d-byte chunk", dropped, size)
	}
	if dropped < size || dropped > total-2*size {
		t.Fatalf("dropped = %d, want between %d and %d", dropped, size, total-2*size)
	}

	// Drain the pipe; every byte that was not dropped must arrive intact.
	// Chunks keep their order (drops only ever remove the oldest), so the
	// fill byte must strictly increase from one chunk to the next.
	want := total - dropped
	if err := remote.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting read deadline: %v", err)
	}
	buf := make([]byte, want)
	if _, err := io.ReadFull(remote, buf); err != nil {
		t.Fatalf("reading %d bytes: %v", want, err)
	}
	last := byte(0)
	for i := 0; i < len(buf); i += size {
		fill := buf[i]
		if fill <= last || fill > chunks {
			t.Fatalf("chunk %d fill = %#x, want increasing fills after %#x", i/size, fill, last)
		}
		for j := i; j < i+size; j++ {
			if buf[j] != fill {
				t.Fatalf("torn chunk %d at byte %d: %#x != %#x", i/size, j, buf[j], fill)
			}
		}
		last = fill
	}
	waitFor(t, 5*time.Second, func() bool {
		count, _, _ := c.q.stats()
		return count == 0
	}, "queue drained")

	// Byte accounting: everything enqueued was either written or dropped.
	if got := int(metricValue(t, m, metrics.NameClientBytesTotal)); got != want {
		t.Errorf("client_bytes_total = %d, want %d", got, want)
	}
	if dropped+want != total {
		t.Errorf("dropped %d + written %d != enqueued %d", dropped, want, total)
	}
}

func TestClientWriteErrorEndsRun(t *testing.T) {
	c, remote, done := startClient(t, 1024)
	c.enqueue([]byte("hello"))
	_ = remote.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not exit after the connection broke")
	}
}

func TestClientCloseUnblocksWriterAndIsIdempotent(t *testing.T) {
	local, remote := net.Pipe()
	c := newClient(7, local, 1024, 0, metrics.New(), testLogger(t))
	done := make(chan struct{})
	go func() {
		c.run()
		close(done)
	}()
	c.enqueue(chunk(1024, 0xaa)) // blocks: nothing reads remote
	c.close()
	c.close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not unblock the writer")
	}
	if _, err := remote.Write([]byte{1}); err == nil {
		t.Error("write to a closed client succeeded")
	}
}

func TestStalled(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		chunks  int
		age     time.Duration
		timeout time.Duration
		want    bool
	}{
		{"quiet client never stalls", 0, time.Hour, time.Minute, false},
		{"draining client is healthy", 5, 2 * time.Second, time.Minute, false},
		{"stuck client stalls", 5, 2 * time.Minute, time.Minute, true},
		{"disabled timeout never stalls", 5, 2 * time.Minute, 0, false},
		{"exactly at the boundary", 5, time.Minute, time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stalled(tc.chunks, now.Add(-tc.age), now, tc.timeout)
			if got != tc.want {
				t.Errorf("stalled(%d, age %s, timeout %s) = %v, want %v",
					tc.chunks, tc.age, tc.timeout, got, tc.want)
			}
		})
	}
}
