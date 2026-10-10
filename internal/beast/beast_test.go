package beast

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"math/rand/v2"
	"testing"
)

// mustHex decodes a hex string for use as a frame payload.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

// mustEncode encodes a frame or fails the test.
func mustEncode(t *testing.T, f Frame) []byte {
	t.Helper()
	raw, err := Encode(f)
	if err != nil {
		t.Fatalf("Encode(%#x): %v", f.Type, err)
	}
	return raw
}

// apPayload builds an Address/Parity payload (DF4/5/16/20/21/24-31) whose
// CRC syndrome decodes back to icao, the same way the transponder computes
// the parity field.
func apPayload(df int, icao uint32, n int) []byte {
	p := make([]byte, n)
	p[0] = byte(df << 3)
	for i := 1; i < n-3; i++ {
		p[i] = byte(i * 7)
	}
	par := Syndrome(append(append([]byte(nil), p[:n-3]...), 0, 0, 0))
	par ^= icao
	p[n-3], p[n-2], p[n-1] = byte(par>>16), byte(par>>8), byte(par)
	return p
}

// aaPayload builds a payload carrying the address in bits 9-32 (DF11/17/18).
func aaPayload(df int, icao uint32, n int) []byte {
	p := make([]byte, n)
	p[0] = byte(df<<3) | 5
	p[1], p[2], p[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	for i := 4; i < n; i++ {
		p[i] = byte(i * 3)
	}
	return p
}

func TestPayloadLenFor(t *testing.T) {
	cases := []struct {
		typ  byte
		want int
		ok   bool
	}{
		{TypeModeAC, 2, true},
		{TypeModeSShort, 7, true},
		{TypeModeSLong, 14, true},
		{TypePosition, 21, true},
		{'0', 0, false},
		{'9', 0, false},
		{0x1a, 0, false},
	}
	for _, tc := range cases {
		got, ok := payloadLenFor(tc.typ)
		if got != tc.want || ok != tc.ok {
			t.Errorf("payloadLenFor(%#x) = (%d, %v), want (%d, %v)", tc.typ, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEscapeAppendsDoubledMarker(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"no markers", []byte{0x00, 0xff, 0x41}, []byte{0x00, 0xff, 0x41}},
		{"single marker", []byte{0x01, 0x1a, 0x02}, []byte{0x01, 0x1a, 0x1a, 0x02}},
		{"all markers", []byte{0x1a, 0x1a}, []byte{0x1a, 0x1a, 0x1a, 0x1a}},
		{"marker at end", []byte{0x42, 0x1a}, []byte{0x42, 0x1a, 0x1a}},
		{"empty", nil, []byte{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AppendEscaped(nil, tc.in)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("AppendEscaped(% x) = % x, want % x", tc.in, got, tc.want)
			}
			if n := EscapedLen(tc.in); n != len(got) {
				t.Errorf("EscapedLen = %d, want %d", n, len(got))
			}
		})
	}
}

func TestEscapeUnescapeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"plain", []byte("hello beast")},
		{"markers everywhere", bytes.Repeat([]byte{0x1a}, 32)},
		{"mixed", []byte{0x1a, 0x00, 0x1a, 0x1a, 0xff, 0x1a}},
		{"empty", nil},
	}
	rng := rand.New(rand.NewPCG(42, 43))
	for i := 0; i < 100; i++ {
		n := 1 + rng.IntN(64)
		b := make([]byte, n)
		for j := range b {
			if j%4 == 0 {
				b[j] = 0x1a
				continue
			}
			b[j] = byte(rng.IntN(256))
		}
		cases = append(cases, struct {
			name string
			in   []byte
		}{name: "random", in: b})
	}
	for _, tc := range cases {
		escaped := AppendEscaped(nil, tc.in)
		got, err := unescape(escaped)
		if err != nil {
			t.Fatalf("unescape: %v", err)
		}
		if !bytes.Equal(got, tc.in) {
			t.Fatalf("round trip: got % x, want % x", got, tc.in)
		}
	}
}

func TestUnescapeRejectsBareMarker(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"trailing marker", []byte{0x01, 0x1a}},
		{"marker then other", []byte{0x01, 0x1a, 0x02}},
		{"only marker", []byte{0x1a}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := unescape(tc.in); !errors.Is(err, ErrBadEscape) {
				t.Errorf("unescape(% x) err = %v, want ErrBadEscape", tc.in, err)
			}
		})
	}
}

func TestEncodeParseRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
	}{
		{"mode ac heartbeat", Frame{Type: TypeModeAC, Payload: []byte{0, 0}}},
		{"escaped everything", Frame{
			Type:    TypeModeSShort,
			MLAT:    0x1a1a1a1a1a1a,
			Signal:  0x1a,
			Payload: bytes.Repeat([]byte{0x1a}, 7),
		}},
		{"mode s long df17", Frame{
			Type:    TypeModeSLong,
			MLAT:    0x000102030405,
			Signal:  200,
			Payload: aaPayload(17, 0x4840d6, 14),
		}},
		{"radarcape position", Frame{
			Type:    TypePosition,
			MLAT:    0x112233445566,
			Signal:  128,
			Payload: bytes.Repeat([]byte{0x42}, 21),
		}},
	}
	rng := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 50; i++ {
		types := []byte{TypeModeAC, TypeModeSShort, TypeModeSLong, TypePosition}
		typ := types[rng.IntN(len(types))]
		plen, _ := payloadLenFor(typ)
		p := make([]byte, plen)
		for j := range p {
			p[j] = byte(rng.IntN(256))
		}
		cases = append(cases, struct {
			name string
			f    Frame
		}{
			name: "random",
			f: Frame{
				Type:    typ,
				MLAT:    rng.Uint64() & 0xffffffffffff,
				Signal:  byte(rng.IntN(256)),
				Payload: p,
			},
		})
	}
	for _, tc := range cases {
		raw := mustEncode(t, tc.f)
		got, err := Parse(raw)
		if err != nil {
			t.Fatalf("%s: Parse: %v", tc.name, err)
		}
		if got.Type != tc.f.Type || got.MLAT != tc.f.MLAT || got.Signal != tc.f.Signal {
			t.Errorf("%s: envelope = {type %#x mlat %#x signal %#x}, want {type %#x mlat %#x signal %#x}",
				tc.name, got.Type, got.MLAT, got.Signal, tc.f.Type, tc.f.MLAT, tc.f.Signal)
		}
		if !bytes.Equal(got.Payload, tc.f.Payload) {
			t.Errorf("%s: payload = % x, want % x", tc.name, got.Payload, tc.f.Payload)
		}
	}
}

func TestParseFrameFields(t *testing.T) {
	// The classic ICAO example message, carried inside a frame whose MLAT
	// and signal bytes contain 0x1a and must therefore be escaped.
	f := Frame{
		Type:    TypeModeSLong,
		MLAT:    0x1a2b3c4d5e6f,
		Signal:  0x1a,
		Payload: mustHex(t, "8d4840d6202cc371c32ce0576098"),
	}
	f.Payload[4], f.Payload[5] = 0x1a, 0x1a // force escapes in the payload too
	raw := mustEncode(t, f)

	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Type != TypeModeSLong {
		t.Errorf("Type = %#x, want %#x", got.Type, TypeModeSLong)
	}
	if got.MLAT != 0x1a2b3c4d5e6f {
		t.Errorf("MLAT = %#x, want %#x", got.MLAT, 0x1a2b3c4d5e6f)
	}
	if got.Signal != 0x1a {
		t.Errorf("Signal = %#x, want %#x", got.Signal, 0x1a)
	}
	if !bytes.Equal(got.Payload, f.Payload) {
		t.Errorf("Payload = % x, want % x", got.Payload, f.Payload)
	}
	if df := got.DF(); df != 17 {
		t.Errorf("DF = %d, want 17", df)
	}
	if icao, ok := got.ICAO(); !ok || icao != 0x4840d6 {
		t.Errorf("ICAO = (%#x, %v), want (0x4840d6, true)", icao, ok)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	valid := mustEncode(t, Frame{Type: TypeModeSShort, Payload: aaPayload(11, 0xabcdef, 7)})
	bareEscape := append([]byte{Marker, TypeModeSShort}, 0, 0, 0, 0, 0, 0, 0x1a, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07)

	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		{"empty", nil, ErrLength},
		{"marker only", []byte{Marker}, ErrLength},
		{"missing marker", []byte{0x1b, TypeModeSShort}, ErrNotFrame},
		{"unknown type", []byte{Marker, '9', 0, 0, 0, 0, 0, 0, 0}, ErrUnknownType},
		{"truncated body", valid[:len(valid)-1], ErrLength},
		{"bare escape mid frame", bareEscape, ErrBadEscape},
		{"extra trailing byte", append(append([]byte(nil), valid...), 0x00), ErrLength},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.raw)
			if !errors.Is(err, tc.want) {
				t.Errorf("Parse err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFrameDF(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
		want int
	}{
		{"df17", Frame{Type: TypeModeSLong, Payload: aaPayload(17, 0x4840d6, 14)}, 17},
		{"df11", Frame{Type: TypeModeSShort, Payload: aaPayload(11, 0xabcdef, 7)}, 11},
		{"df4", Frame{Type: TypeModeSShort, Payload: apPayload(4, 0xabc123, 7)}, 4},
		{"mode ac", Frame{Type: TypeModeAC, Payload: []byte{0x12, 0x34}}, -1},
		{"position", Frame{Type: TypePosition, Payload: make([]byte, 21)}, -1},
		{"no payload", Frame{Type: TypeModeSLong}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.f.DF(); got != tc.want {
				t.Errorf("DF() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFrameICAO(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
		want uint32
		ok   bool
	}{
		{"df17 announced address", Frame{Type: TypeModeSLong, Payload: aaPayload(17, 0x4840d6, 14)}, 0x4840d6, true},
		{"df11 announced address", Frame{Type: TypeModeSShort, Payload: aaPayload(11, 0xabcdef, 7)}, 0xabcdef, true},
		{"df18 announced address", Frame{Type: TypeModeSLong, Payload: aaPayload(18, 0x3ca9f1, 14)}, 0x3ca9f1, true},
		{"df4 address parity", Frame{Type: TypeModeSShort, Payload: apPayload(4, 0xabc123, 7)}, 0xabc123, true},
		{"df20 address parity", Frame{Type: TypeModeSShort, Payload: apPayload(20, 0x4ca123, 7)}, 0x4ca123, true},
		{"df16 long address parity", Frame{Type: TypeModeSLong, Payload: apPayload(16, 0x39ab12, 14)}, 0x39ab12, true},
		{"df19 undecodable", Frame{Type: TypeModeSLong, Payload: aaPayload(19, 0x111111, 14)}, 0, false},
		{"mode ac has no address", Frame{Type: TypeModeAC, Payload: []byte{0x12, 0x34}}, 0, false},
		{"position frame has no address", Frame{Type: TypePosition, Payload: make([]byte, 21)}, 0, false},
		{"short payload", Frame{Type: TypeModeSLong, Payload: []byte{0x8d, 0x48}}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.f.ICAO()
			if got != tc.want || ok != tc.ok {
				t.Errorf("ICAO() = (%#x, %v), want (%#x, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestFrameRSSI(t *testing.T) {
	cases := []struct {
		name   string
		signal byte
		want   float64
	}{
		{"full scale", 255, 0},
		{"half power", 128, 20 * math.Log10(128.0/255.0)},
		{"quarter power", 64, 20 * math.Log10(64.0/255.0)},
		{"weak", 16, 20 * math.Log10(16.0/255.0)},
		{"one", 1, 20 * math.Log10(1.0/255.0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := Frame{Type: TypeModeSShort, Signal: tc.signal, Payload: aaPayload(17, 1, 7)}
			got := f.RSSI()
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("RSSI() = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("no signal info is -inf", func(t *testing.T) {
		f := Frame{Type: TypeModeSShort, Signal: 0, Payload: aaPayload(17, 1, 7)}
		if r := f.RSSI(); !math.IsInf(r, -1) {
			t.Errorf("RSSI() = %v, want -Inf", r)
		}
	})
}

func TestSyndromeMatchesDump1090GoldenVector(t *testing.T) {
	// The textbook DF17 message: parity 576098 covers the first 11 bytes,
	// so the syndrome of the whole message is zero and the CRC of the
	// prefix (with zeroed parity) is 576098.
	msg := mustHex(t, "8d4840d6202cc371c32ce0576098")
	if s := Syndrome(msg); s != 0 {
		t.Errorf("Syndrome(whole) = %#06x, want 0", s)
	}
	prefix := append([]byte(nil), msg[:11]...)
	if s := Syndrome(append(prefix, 0, 0, 0)); s != 0x576098 {
		t.Errorf("Syndrome(prefix) = %#06x, want 0x576098", s)
	}
	if s := Syndrome([]byte{0x01}); s != 0 {
		t.Errorf("Syndrome(short input) = %#x, want 0", s)
	}
}
