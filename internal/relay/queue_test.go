package relay

import (
	"bytes"
	"testing"
)

// chunk builds a distinguishable queue payload of n bytes.
func chunk(n int, fill byte) []byte {
	return bytes.Repeat([]byte{fill}, n)
}

func TestQueueDropOldest(t *testing.T) {
	cases := []struct {
		name        string
		maxBytes    int
		pushes      []byte // chunk sizes, one entry each
		wantDropped int
		wantCount   int
		wantBytes   int
		keep        []byte // fill values expected to survive, in order
	}{
		{
			name: "fits without dropping", maxBytes: 100,
			pushes:      []byte{40, 40},
			wantDropped: 0, wantCount: 2, wantBytes: 80,
			keep: []byte{1, 2},
		},
		{
			name: "third push evicts oldest", maxBytes: 100,
			pushes:      []byte{40, 40, 40},
			wantDropped: 40, wantCount: 2, wantBytes: 80,
			keep: []byte{2, 3},
		},
		{
			name: "evicts repeatedly until it fits", maxBytes: 100,
			pushes:      []byte{30, 30, 30, 30},
			wantDropped: 30, wantCount: 3, wantBytes: 90,
			keep: []byte{2, 3, 4},
		},
		{
			name: "one big eviction", maxBytes: 100,
			pushes:      []byte{80, 80},
			wantDropped: 80, wantCount: 1, wantBytes: 80,
			keep: []byte{2},
		},
		{
			name: "empty queue keeps oversized chunk", maxBytes: 50,
			pushes:      []byte{60},
			wantDropped: 0, wantCount: 1, wantBytes: 60,
			keep: []byte{1},
		},
		{
			name: "oversized chunk evicts everything", maxBytes: 50,
			pushes:      []byte{20, 60},
			wantDropped: 20, wantCount: 1, wantBytes: 60,
			keep: []byte{2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := newQueue(tc.maxBytes)
			dropped := 0
			for i, size := range tc.pushes {
				dropped += q.push(chunk(int(size), byte(i+1)))
			}
			if dropped != tc.wantDropped {
				t.Errorf("dropped bytes = %d, want %d", dropped, tc.wantDropped)
			}
			count, bytesNow, cumDropped := q.stats()
			if count != tc.wantCount || bytesNow != tc.wantBytes || cumDropped != uint64(tc.wantDropped) {
				t.Errorf("stats = (%d, %d, %d), want (%d, %d, %d)",
					count, bytesNow, cumDropped, tc.wantCount, tc.wantBytes, tc.wantDropped)
			}
			for i, fill := range tc.keep {
				b, ok := q.pop()
				if !ok {
					t.Fatalf("pop %d on empty queue", i)
				}
				if !bytes.Equal(b, chunk(len(b), fill)) {
					t.Errorf("pop %d fill = %#x, want %#x", i, b[0], fill)
				}
			}
			if _, ok := q.pop(); ok {
				t.Error("queue has more chunks than expected")
			}
		})
	}
}

func TestQueueRingWrapKeepsFIFO(t *testing.T) {
	const chunkSize = 10
	q := newQueue(40) // four 10-byte chunks
	var model []byte  // independent replica of the drop-oldest policy
	seq := byte(0)
	pushed, popped := 0, 0

	for round := 0; round < 50; round++ {
		for i := 0; i < 3; i++ {
			seq++
			for len(model) > 0 && (len(model)+1)*chunkSize > 40 {
				model = model[1:]
			}
			model = append(model, seq)
			q.push(chunk(chunkSize, seq))
			pushed += chunkSize
		}
		for i := 0; i < 2; i++ {
			b, ok := q.pop()
			if !ok {
				t.Fatalf("round %d: pop on empty queue", round)
			}
			if len(model) == 0 || b[0] != model[0] {
				t.Fatalf("round %d: out of order: got %#x, want %#x", round, b[0], model)
			}
			model = model[1:]
			popped += chunkSize
		}
		count, bytesNow, _ := q.stats()
		if count != len(model) || bytesNow != len(model)*chunkSize {
			t.Fatalf("round %d: stats = (%d, %d), model has %d",
				round, count, bytesNow, len(model))
		}
	}
	for len(model) > 0 {
		b, ok := q.pop()
		if !ok {
			t.Fatalf("drain: queue empty with %d expected", len(model))
		}
		if b[0] != model[0] {
			t.Fatalf("drain: got %#x, want %#x", b[0], model[0])
		}
		model = model[1:]
		popped += chunkSize
	}
	_, _, dropped := q.stats()
	if want := uint64(pushed - popped); dropped != want {
		t.Errorf("dropped bytes = %d, want %d (pushed %d, popped %d)", dropped, want, pushed, popped)
	}
}

func TestQueuePopEmpty(t *testing.T) {
	q := newQueue(10)
	if b, ok := q.pop(); ok || b != nil {
		t.Errorf("pop on empty = (%v, %v), want (nil, false)", b, ok)
	}
	q.push(chunk(10, 7))
	if _, ok := q.pop(); !ok {
		t.Fatal("pop failed on non-empty queue")
	}
	if b, ok := q.pop(); ok || b != nil {
		t.Errorf("second pop = (%v, %v), want (nil, false)", b, ok)
	}
}

func TestQueueAccountingAcrossDropEpisodes(t *testing.T) {
	q := newQueue(30)
	q.push(chunk(10, 1))
	q.push(chunk(10, 2))
	if _, _, d := q.stats(); d != 0 {
		t.Fatalf("dropped = %d before any eviction", d)
	}
	q.push(chunk(10, 3)) // 30 == max, still fits
	if count, bytesNow, d := q.stats(); count != 3 || bytesNow != 30 || d != 0 {
		t.Fatalf("stats = (%d, %d, %d), want (3, 30, 0)", count, bytesNow, d)
	}
	q.push(chunk(10, 4)) // evicts fill 1
	q.push(chunk(10, 5)) // evicts fill 2
	if count, bytesNow, d := q.stats(); count != 3 || bytesNow != 30 || d != 20 {
		t.Fatalf("stats = (%d, %d, %d), want (3, 30, 20)", count, bytesNow, d)
	}
	// Drain and refill: the cumulative counter must not reset.
	for i := 0; i < 3; i++ {
		q.pop()
	}
	q.push(chunk(30, 9))
	if _, _, d := q.stats(); d != 20 {
		t.Errorf("cumulative dropped = %d, want 20", d)
	}
}
