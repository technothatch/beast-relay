package beast

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// frameFixture builds an encoded frame of the given type with a
// recognisable payload.
func frameFixture(t *testing.T, typ byte, fill byte) []byte {
	t.Helper()
	plen, ok := payloadLenFor(typ)
	if !ok {
		t.Fatalf("unknown fixture type %#x", typ)
	}
	p := bytes.Repeat([]byte{fill}, plen)
	if typ == TypeModeSShort || typ == TypeModeSLong {
		p[0] = 17 << 3
	}
	return mustEncode(t, Frame{
		Type:    typ,
		MLAT:    0x010203040506,
		Signal:  0x40,
		Payload: p,
	})
}

// collect feeds chunks to a parser and returns everything it produced.
func collect(p *Parser, chunks ...[]byte) []Segment {
	var out []Segment
	for _, c := range chunks {
		out = append(out, p.Feed(c)...)
	}
	return out
}

// frameBytes returns the Raw of every frame segment.
func frameBytes(segs []Segment) [][]byte {
	var out [][]byte
	for _, s := range segs {
		if s.IsFrame {
			out = append(out, s.Raw)
		}
	}
	return out
}

// concat joins segment bytes in order.
func concat(segs []Segment) []byte {
	var out []byte
	for _, s := range segs {
		out = append(out, s.Raw...)
	}
	return out
}

func TestParserSingleFrame(t *testing.T) {
	raw := frameFixture(t, TypeModeSLong, 0x42)
	segs := collect(NewParser(), raw)
	if len(segs) != 1 || !segs[0].IsFrame {
		t.Fatalf("segments = %d (frame flags %v), want exactly one frame", len(segs), frameFlags(segs))
	}
	if !bytes.Equal(segs[0].Raw, raw) {
		t.Errorf("Raw = % x, want % x", segs[0].Raw, raw)
	}
}

func frameFlags(segs []Segment) []bool {
	out := make([]bool, len(segs))
	for i, s := range segs {
		out[i] = s.IsFrame
	}
	return out
}

func TestParserByteAtATime(t *testing.T) {
	garbage := []byte{0xde, 0xad, 0xbe}
	raw := append(append([]byte(nil), garbage...), frameFixture(t, TypeModeSShort, 0x43)...)
	p := NewParser()
	var chunks [][]byte
	for _, b := range raw {
		chunks = append(chunks, []byte{b})
	}
	segs := collect(p, chunks...)
	if got := concat(segs); !bytes.Equal(got, raw) {
		t.Errorf("reassembly = % x, want % x", got, raw)
	}
	frames := frameBytes(segs)
	if len(frames) != 1 {
		t.Fatalf("got %d frame segments, want 1", len(frames))
	}
	if _, err := Parse(frames[0]); err != nil {
		t.Errorf("Parse: %v", err)
	}
}

func TestParserSplitBoundaries(t *testing.T) {
	f1 := frameFixture(t, TypeModeSShort, 0x11)
	f2 := frameFixture(t, TypeModeSLong, 0x22)
	stream := append(append([]byte(nil), f1...), f2...)
	for split := 1; split < len(stream); split++ {
		t.Run(fmt.Sprintf("split-at-%d", split), func(t *testing.T) {
			segs := collect(NewParser(), stream[:split], stream[split:])
			frames := frameBytes(segs)
			if len(frames) != 2 {
				t.Fatalf("got %d frame segments, want 2", len(frames))
			}
			if !bytes.Equal(frames[0], f1) || !bytes.Equal(frames[1], f2) {
				t.Errorf("frames = [% x | % x], want [% x | % x]", frames[0], frames[1], f1, f2)
			}
		})
	}
}

func TestParserHandlesManyFramesAndGarbage(t *testing.T) {
	g1 := []byte{0x00, 0xff}
	g2 := []byte{0x41, 0x42, 0x43}
	f1 := frameFixture(t, TypeModeAC, 0x01)
	f2 := frameFixture(t, TypeModeSLong, 0x02)
	f3 := frameFixture(t, TypeModeSShort, 0x03)
	stream := concat([]Segment{
		{Raw: g1}, {Raw: f1}, {Raw: g2}, {Raw: f2}, {Raw: f3},
	})
	segs := collect(NewParser(), stream)
	frames := frameBytes(segs)
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	for i, want := range [][]byte{f1, f2, f3} {
		if !bytes.Equal(frames[i], want) {
			t.Errorf("frame %d mismatch", i)
		}
	}
	if got := concat(segs); !bytes.Equal(got, stream) {
		t.Errorf("byte preservation broken: % x != % x", got, stream)
	}
}

func TestParserKeepsBareMarkersInGarbage(t *testing.T) {
	// 0x1a followed by a non-type byte must never be mistaken for a frame.
	garbage := []byte{0x00, 0x1a, 0x00, 0x1a, 0x99, 0xff}
	frame := frameFixture(t, TypeModeSShort, 0x21)
	stream := append(append([]byte(nil), garbage...), frame...)
	segs := collect(NewParser(), stream)
	if len(segs) == 0 {
		t.Fatal("no segments")
	}
	if segs[0].IsFrame {
		t.Error("garbage was classified as a frame")
	}
	if !bytes.Equal(segs[0].Raw, garbage) {
		t.Errorf("garbage = % x, want % x", segs[0].Raw, garbage)
	}
	if frames := frameBytes(segs); len(frames) != 1 || !bytes.Equal(frames[0], frame) {
		t.Errorf("frame not recovered after garbage")
	}
}

func TestParserHoldsTrailingMarker(t *testing.T) {
	// Noise ending in 0x1a: the marker is held until its successor byte
	// arrives, because it might start a frame.
	p := NewParser()
	segs := p.Feed([]byte{0x01, Marker})
	if len(segs) != 1 || segs[0].IsFrame || !bytes.Equal(segs[0].Raw, []byte{0x01}) {
		t.Fatalf("segments = %+v, want only the 0x01 noise byte", segs)
	}
	// A held marker followed by a non-type byte was noise after all.
	segs = p.Feed([]byte{0x99})
	if len(segs) != 1 || segs[0].IsFrame || !bytes.Equal(segs[0].Raw, []byte{Marker, 0x99}) {
		t.Fatalf("segments = %+v, want the marker flushed as noise", segs)
	}
	// A frame split just before its last byte reassembles once it lands.
	raw := frameFixture(t, TypeModeSShort, 0x31)
	p2 := NewParser()
	p2.Feed(raw[:len(raw)-1])
	segs = p2.Feed(raw[len(raw)-1:])
	if frames := frameBytes(segs); len(frames) != 1 || !bytes.Equal(frames[0], raw) {
		t.Errorf("frame not reassembled across a chunk boundary, got %d frames", len(frames))
	}
}

func TestParserResyncsAfterMalformedEscape(t *testing.T) {
	// A 0x1a in the body that is not doubled corrupts the frame; the
	// parser must hand over the truncated frame (which will fail Parse and
	// be forwarded), then keep the offending marker for a fresh scan.
	malformed := append([]byte{Marker, TypeModeSLong}, 0, 0, 0, 0, 0, 0) // mlat done
	malformed = append(malformed, 0x1a, 0x7e)                            // bare escape
	good := frameFixture(t, TypeModeSShort, 0x51)
	stream := append(append([]byte(nil), malformed...), good...)

	segs := collect(NewParser(), stream)
	if got := concat(segs); !bytes.Equal(got, stream) {
		t.Errorf("byte preservation broken: % x != % x", got, stream)
	}
	frames := frameBytes(segs)
	if len(frames) < 2 {
		t.Fatalf("got %d frame segments, want the truncated one plus the good one", len(frames))
	}
	if _, err := Parse(frames[0]); err == nil {
		t.Error("truncated frame parsed cleanly, want error")
	} else if !errors.Is(err, ErrLength) {
		t.Errorf("truncated frame error = %v, want ErrLength", err)
	}
	last := frames[len(frames)-1]
	if _, err := Parse(last); err != nil {
		t.Errorf("frame after resync: %v", err)
	}
}

func TestParserSelectiveForwardPreservesBytes(t *testing.T) {
	// Simulates the relay's filter path: drop every type-'3' frame, keep
	// everything else. What is kept must be exactly the input minus the
	// dropped frames.
	f1 := frameFixture(t, TypeModeSShort, 0x61)
	f2 := frameFixture(t, TypeModeSLong, 0x62) // to be dropped
	f3 := frameFixture(t, TypeModeAC, 0x63)
	garbage := []byte{0x7f, 0x00, 0x1a, 0x80}
	stream := concat([]Segment{{Raw: garbage}, {Raw: f1}, {Raw: f2}, {Raw: f3}, {Raw: garbage}})

	var chunks [][]byte
	for _, b := range stream {
		chunks = append(chunks, []byte{b})
	}
	segs := collect(NewParser(), chunks...)

	var kept []byte
	for _, s := range segs {
		if s.IsFrame {
			fr, err := Parse(s.Raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if fr.Type == TypeModeSLong {
				continue // dropped
			}
		}
		kept = append(kept, s.Raw...)
	}
	want := concat([]Segment{{Raw: garbage}, {Raw: f1}, {Raw: f3}, {Raw: garbage}})
	if !bytes.Equal(kept, want) {
		t.Errorf("kept % x, want % x", kept, want)
	}
}
