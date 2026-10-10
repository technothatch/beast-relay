// Package beast implements the Mode S "Beast" binary wire protocol that
// dump1090-fa and readsb speak on TCP port 30005 (beast output).
//
// A frame starts with the marker byte 0x1a followed by a type byte: '1' for
// Mode A/C, '2' for a 56-bit Mode S message, '3' for a 112-bit Mode S
// message and '5' for a Radarcape position message. The marker and type are
// followed by a 48-bit big-endian MLAT timestamp, one signal byte and the
// message payload. Every 0x1a byte after the marker is doubled on the wire
// ("byte stuffing"), so a data byte can never be confused with a marker.
//
// The relay only decodes frames when a filter is configured; without one the
// stream is forwarded verbatim. Parse therefore works on raw wire bytes and
// validates the escaping itself rather than trusting a pre-unescaped copy.
package beast

import (
	"errors"
	"fmt"
	"math"
)

// Frame type bytes as emitted by dump1090-fa and readsb.
const (
	// Marker starts every frame.
	Marker = 0x1a
	// TypeModeAC is a Mode A/C message (2 payload bytes).
	TypeModeAC = '1'
	// TypeModeSShort is a 56-bit Mode S message (7 payload bytes).
	TypeModeSShort = '2'
	// TypeModeSLong is a 112-bit Mode S message (14 payload bytes).
	TypeModeSLong = '3'
	// TypePosition is a Radarcape position message (21 payload bytes).
	TypePosition = '5'
)

// Fixed sizes of the frame envelope, in wire order.
const (
	headerLen     = 2 // marker + type byte
	mlatLen       = 6
	signalLen     = 1
	payloadModeAC = 2
	payloadShort  = 7
	payloadLong   = 14
	payloadPos    = 21
)

// MaxFrameLen is the largest frame on the wire: envelope plus the longest
// payload with every body byte escaped.
const MaxFrameLen = headerLen + 2*(mlatLen+signalLen+payloadPos)

// Decoding failures. Callers that only forward bytes treat these as "not
// decodable" and pass the bytes through unchanged.
var (
	// ErrNotFrame is returned when the raw bytes do not start with 0x1a.
	ErrNotFrame = errors.New("beast: missing 0x1a frame marker")
	// ErrUnknownType is returned for a type byte other than '1','2','3','5'.
	ErrUnknownType = errors.New("beast: unknown message type")
	// ErrBadEscape is returned for a 0x1a not followed by another 0x1a.
	ErrBadEscape = errors.New("beast: malformed 0x1a escape")
	// ErrLength is returned when a frame carries too few or too many bytes.
	ErrLength = errors.New("beast: payload length does not match message type")
)

// Frame is one decoded Beast message.
type Frame struct {
	// Type is the frame type byte (TypeModeAC, TypeModeSShort, ...).
	Type byte
	// MLAT is the 48-bit big-endian master clock timestamp.
	MLAT uint64
	// Signal is the raw signal byte: linear power is (Signal/255)^2, which
	// decoders report as rssi = 10*log10(power) dBFS.
	Signal byte
	// Payload is the message body: 2, 7, 14 or 21 bytes for TypeModeAC,
	// TypeModeSShort, TypeModeSLong and TypePosition respectively.
	Payload []byte
}

// payloadLenFor returns the payload size of a frame type.
func payloadLenFor(t byte) (int, bool) {
	switch t {
	case TypeModeAC:
		return payloadModeAC, true
	case TypeModeSShort:
		return payloadShort, true
	case TypeModeSLong:
		return payloadLong, true
	case TypePosition:
		return payloadPos, true
	default:
		return 0, false
	}
}

// AppendEscaped appends b to dst with every 0x1a byte doubled, as the wire
// format requires for everything after the frame marker.
func AppendEscaped(dst, b []byte) []byte {
	for _, c := range b {
		dst = append(dst, c)
		if c == Marker {
			dst = append(dst, Marker)
		}
	}
	return dst
}

// EscapedLen returns the number of wire bytes b occupies after escaping.
func EscapedLen(b []byte) int {
	n := len(b)
	for _, c := range b {
		if c == Marker {
			n++
		}
	}
	return n
}

// unescape collapses doubled 0x1a bytes into literal data bytes. It fails on
// a marker that is not doubled, which only happens on a corrupted stream.
func unescape(src []byte) ([]byte, error) {
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == Marker {
			if i+1 >= len(src) || src[i+1] != Marker {
				return nil, ErrBadEscape
			}
			i++
		}
		out = append(out, c)
	}
	return out, nil
}

// Parse decodes one complete frame, marker included. The payload is a fresh
// copy, so the caller may keep the frame after the input is recycled.
func Parse(raw []byte) (Frame, error) {
	if len(raw) < headerLen {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrLength, len(raw))
	}
	if raw[0] != Marker {
		return Frame{}, ErrNotFrame
	}
	plen, ok := payloadLenFor(raw[1])
	if !ok {
		return Frame{}, fmt.Errorf("%w: %#x", ErrUnknownType, raw[1])
	}
	body, err := unescape(raw[headerLen:])
	if err != nil {
		return Frame{}, err
	}
	if len(body) != mlatLen+signalLen+plen {
		return Frame{}, fmt.Errorf("%w: got %d body bytes, want %d", ErrLength, len(body), mlatLen+signalLen+plen)
	}
	var mlat uint64
	for _, c := range body[:mlatLen] {
		mlat = mlat<<8 | uint64(c)
	}
	payload := make([]byte, plen)
	copy(payload, body[mlatLen+signalLen:])
	return Frame{Type: raw[1], MLAT: mlat, Signal: body[mlatLen], Payload: payload}, nil
}

// Encode renders a frame as wire bytes, escaping the body. It is the inverse
// of Parse and is how tests build realistic traffic.
func Encode(f Frame) ([]byte, error) {
	plen, ok := payloadLenFor(f.Type)
	if !ok {
		return nil, fmt.Errorf("%w: %#x", ErrUnknownType, f.Type)
	}
	if len(f.Payload) != plen {
		return nil, fmt.Errorf("%w: got %d payload bytes, want %d", ErrLength, len(f.Payload), plen)
	}
	var mlat [mlatLen]byte
	v := f.MLAT
	for i := mlatLen - 1; i >= 0; i-- {
		mlat[i] = byte(v)
		v >>= 8
	}
	out := make([]byte, 0, headerLen+EscapedLen(mlat[:])+1+EscapedLen(f.Payload))
	out = append(out, Marker, f.Type)
	out = AppendEscaped(out, mlat[:])
	out = AppendEscaped(out, []byte{f.Signal})
	out = AppendEscaped(out, f.Payload)
	return out, nil
}

// isModeS reports whether the frame carries a Mode S message with a DF field
// and (possibly) a decodable address.
func (f Frame) isModeS() bool {
	return f.Type == TypeModeSShort || f.Type == TypeModeSLong
}

// DF returns the 5-bit Downlink Format of a Mode S payload, or -1 for Mode
// A/C and position frames, which have no DF field.
func (f Frame) DF() int {
	if !f.isModeS() || len(f.Payload) == 0 {
		return -1
	}
	return int(f.Payload[0] >> 3)
}

// ICAO returns the 24-bit ICAO address of the reply, using the same rules
// as dump1090: DF11/17/18 carry the address in bits 9-32 (payload bytes
// 1-3), while Address/Parity formats (DF0/4/5/16/20/21 and DF24-31) encode
// it as the CRC syndrome of the whole message. ok is false when the frame
// has no decodable address (Mode A/C, position frames, unsupported DFs).
func (f Frame) ICAO() (uint32, bool) {
	if !f.isModeS() || len(f.Payload) < 4 {
		return 0, false
	}
	p := f.Payload
	switch df := p[0] >> 3; {
	case df == 11 || df == 17 || df == 18:
		return uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3]), true
	case df == 0 || df == 4 || df == 5 || df == 16 || df == 20 || df == 21 || df >= 24:
		return Syndrome(p), true
	default:
		return 0, false
	}
}

// RSSI returns the signal level in dBFS, matching the rssi field of
// aircraft.json: the signal byte encodes linear power as (byte/255)^2.
// A zero signal byte means "no signal information" and yields -Inf, which
// never passes an active --filter-min-rssi.
func (f Frame) RSSI() float64 {
	if f.Signal == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(float64(f.Signal)/255)
}
