package beast

// Parser splits a Beast byte stream into complete frames while preserving
// every byte it reads. Feed returns ordered segments of the input:
//
//   - IsFrame == false segments are framing noise (bytes between frames)
//     and are always forwarded untouched.
//   - IsFrame == true segments are candidate frames; the caller runs Parse
//     and decides whether to forward them.
//
// Concatenating the bytes of the segments the caller keeps reproduces the
// input exactly, minus only the frames the caller chose to drop. Frame
// boundaries may fall anywhere in a read, so the parser buffers partial
// frames across Feed calls; that buffer stays bounded by one frame because
// everything before a frame candidate is emitted immediately.
type Parser struct {
	pending []byte
}

// NewParser returns a parser with no buffered bytes.
func NewParser() *Parser {
	return &Parser{}
}

// Segment is one contiguous piece of the input stream.
type Segment struct {
	// Raw are the original wire bytes, escapes included.
	Raw []byte
	// IsFrame marks a candidate frame for the caller to Parse.
	IsFrame bool
}

// indexOfFrameStart returns the offset of the next 0x1a marker followed by
// a known type byte, or -1 when the stream holds no candidate. A trailing
// 0x1a is never a candidate: its successor byte has not arrived yet.
func indexOfFrameStart(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] != Marker {
			continue
		}
		if _, ok := payloadLenFor(b[i+1]); ok {
			return i
		}
	}
	return -1
}

// frameWireLen walks the body after the marker and type (b must start at
// the first body byte) and reports how many wire bytes the frame occupies.
//
// complete is true once need logical body bytes were consumed. malformed
// means a 0x1a was followed by something other than 0x1a mid-frame; wireEnd
// then points at the offending marker, which stays pending so scanning can
// resume there, and the truncated frame is handed to the caller (whose
// Parse will fail, and who will forward the bytes).
func frameWireLen(b []byte, need int) (wireEnd int, complete, malformed bool) {
	i, logical := 0, 0
	for logical < need {
		if i >= len(b) {
			return 0, false, false
		}
		if b[i] == Marker {
			if i+1 >= len(b) {
				return 0, false, false
			}
			if b[i+1] != Marker {
				return i, false, true
			}
			i += 2
		} else {
			i++
		}
		logical++
	}
	return i, true, false
}

// Feed appends b to the stream and returns every segment that can be
// decided without further bytes. The returned Raw slices are fresh copies
// and stay valid after the next Feed call.
func (p *Parser) Feed(b []byte) []Segment {
	p.pending = append(p.pending, b...)
	var out []Segment
	for len(p.pending) > 0 {
		idx := indexOfFrameStart(p.pending)
		if idx < 0 {
			// Nothing decidable: flush everything except a possible
			// frame start ending in the last byte.
			cut := len(p.pending)
			if p.pending[cut-1] == Marker {
				cut--
			}
			if cut > 0 {
				out = append(out, Segment{Raw: p.take(cut)})
			}
			break
		}
		if idx > 0 {
			out = append(out, Segment{Raw: p.take(idx)})
		}
		// A candidate now sits at offset 0.
		plen, _ := payloadLenFor(p.pending[1])
		end, complete, malformed := frameWireLen(p.pending[2:], mlatLen+signalLen+plen)
		switch {
		case complete:
			out = append(out, Segment{Raw: p.take(headerLen + end), IsFrame: true})
		case malformed:
			out = append(out, Segment{Raw: p.take(headerLen + end), IsFrame: true})
		default: // incomplete: wait for more bytes
			return out
		}
	}
	return out
}

// take removes the first n bytes from the buffer and returns a copy of
// them.
func (p *Parser) take(n int) []byte {
	out := append([]byte(nil), p.pending[:n]...)
	p.pending = p.pending[n:]
	return out
}
