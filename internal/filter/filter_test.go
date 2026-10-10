package filter

import (
	"testing"

	"github.com/technothatch/beast-relay/internal/beast"
)

// modeS builds a Mode S frame carrying icao. For DF11/17/18 (all-call,
// squitter) the address lives in payload bytes 1-3. Address/Parity formats
// (DF0/4/5/16/20/21, DF24+) instead carry it as the CRC syndrome, so the
// parity field is set to CRC ^ address to make the frame realistic.
func modeS(df int, icao uint32, signal byte) beast.Frame {
	p := make([]byte, 14)
	p[0] = byte(df<<3) | 5
	p[1], p[2], p[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	if df == 0 || df == 4 || df == 5 || df == 16 || df == 20 || df == 21 || df >= 24 {
		crc := beast.Syndrome(p) // zeroed parity: the syndrome is the plain CRC
		p[11] = byte(crc>>16) ^ byte(icao>>16)
		p[12] = byte(crc>>8) ^ byte(icao>>8)
		p[13] = byte(crc) ^ byte(icao)
	}
	return beast.Frame{Type: beast.TypeModeSLong, Signal: signal, Payload: p}
}

// modeAC builds a frame with no decodable address.
func modeAC() beast.Frame {
	return beast.Frame{Type: beast.TypeModeAC, Payload: []byte{0x12, 0x34}}
}

func mustNew(t *testing.T, cfg Config) *Filter {
	t.Helper()
	f, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	return f
}

func TestParseHexList(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		rules   int
		first   rule
		wantErr bool
	}{
		{name: "empty", in: "", rules: 0},
		{name: "single address", in: "4ca123", rules: 1, first: rule{value: 0x4ca123, mask: 0xffffff}},
		{name: "comma separated", in: "4ca123,abc123", rules: 2, first: rule{value: 0x4ca123, mask: 0xffffff}},
		{name: "mixed separators and case", in: "4CA123 abc123;0xABC123", rules: 3, first: rule{value: 0x4ca123, mask: 0xffffff}},
		{name: "prefix match", in: "4ca", rules: 1, first: rule{value: 0x4ca000, mask: 0xfff000}},
		{name: "single digit", in: "a", rules: 1, first: rule{value: 0xa00000, mask: 0xf00000}},
		{name: "non hex", in: "4cg123", wantErr: true},
		{name: "too long", in: "abcdef1", wantErr: true},
		{name: "bare prefix", in: "0x", wantErr: true},
		{name: "one bad entry spoils the list", in: "4ca123,zzz", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules, err := parseHexList(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("parseHexList succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHexList: %v", err)
			}
			if len(rules) != tc.rules {
				t.Fatalf("got %d rules, want %d", len(rules), tc.rules)
			}
			if tc.rules > 0 && rules[0] != tc.first {
				t.Errorf("first rule = %+v, want %+v", rules[0], tc.first)
			}
		})
	}
}

func TestFilterInactiveAcceptsAll(t *testing.T) {
	var nilFilter *Filter
	if !nilFilter.Accept(modeS(17, 0x4ca123, 200)) {
		t.Error("nil filter rejected a frame")
	}
	f := mustNew(t, Config{})
	if f.Active() {
		t.Error("empty filter reports Active")
	}
	for _, fr := range []beast.Frame{modeS(17, 0x4ca123, 0), modeAC(), {Type: beast.TypePosition, Payload: make([]byte, 21)}} {
		if !f.Accept(fr) {
			t.Errorf("inactive filter rejected type %#x", fr.Type)
		}
	}
}

func TestFilterActive(t *testing.T) {
	min := -10.0
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"nothing set", Config{}, false},
		{"min rssi", Config{MinRSSI: &min}, true},
		{"allow only", Config{AllowHex: "4ca123"}, true},
		{"deny only", Config{DenyHex: "4ca123"}, true},
		{"all three", Config{MinRSSI: &min, AllowHex: "4ca123", DenyHex: "abc"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustNew(t, tc.cfg).Active(); got != tc.want {
				t.Errorf("Active() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFilterDenyHex(t *testing.T) {
	cases := []struct {
		name  string
		deny  string
		frame beast.Frame
		want  bool
	}{
		{"listed address dropped", "4ca123", modeS(17, 0x4ca123, 200), false},
		{"other address kept", "4ca123", modeS(17, 0xabc123, 200), true},
		{"prefix drops whole block", "4ca", modeS(11, 0x4ca9f0, 200), false},
		{"prefix outside block kept", "4ca", modeS(11, 0x4cb123, 200), true},
		{"mode ac never accused", "4ca123", modeAC(), true},
		{"ap format denied too", "abc123", modeS(20, 0xabc123, 200), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := mustNew(t, Config{DenyHex: tc.deny})
			if got := f.Accept(tc.frame); got != tc.want {
				t.Errorf("Accept = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFilterAllowHex(t *testing.T) {
	cases := []struct {
		name  string
		allow string
		frame beast.Frame
		want  bool
	}{
		{"listed address kept", "4ca123", modeS(17, 0x4ca123, 200), true},
		{"other address dropped", "4ca123", modeS(17, 0xabc123, 200), false},
		{"prefix allows whole block", "4ca", modeS(17, 0x4ca1b2, 200), true},
		{"prefix outside block dropped", "4ca", modeS(17, 0xabc123, 200), false},
		{"addressless frames dropped under allow", "4ca123", modeAC(), false},
		{"addressless position dropped under allow", "4ca123", beast.Frame{Type: beast.TypePosition, Payload: make([]byte, 21)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := mustNew(t, Config{AllowHex: tc.allow})
			if got := f.Accept(tc.frame); got != tc.want {
				t.Errorf("Accept = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFilterMinRSSI(t *testing.T) {
	min := -10.0
	f := mustNew(t, Config{MinRSSI: &min})

	// Expectations are derived from the frame's own RSSI, so the table
	// stays correct if the byte-to-dBFS curve is ever revisited.
	cases := []struct {
		name   string
		signal byte
	}{
		{"full scale kept", 255},  // 0 dBFS
		{"mid kept", 128},         // ~ -6 dBFS
		{"weak dropped", 64},      // ~ -12 dBFS
		{"very weak dropped", 16}, // ~ -24 dBFS
		{"no signal info dropped", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := modeS(17, 0x4ca123, tc.signal)
			want := fr.RSSI() >= min
			if got := f.Accept(fr); got != want {
				t.Errorf("Accept(signal=%d) = %v, want %v (rssi %.2f dBFS, min %.2f)",
					tc.signal, got, want, fr.RSSI(), min)
			}
		})
	}
	t.Run("below threshold dropped", func(t *testing.T) {
		if f.Accept(modeS(17, 0x4ca123, 64)) { // -12.0 dBFS
			t.Error("weak frame accepted with min -10 dBFS")
		}
	})
	t.Run("zero threshold applies", func(t *testing.T) {
		zero := 0.0
		fz := mustNew(t, Config{MinRSSI: &zero})
		if !fz.Accept(modeS(17, 0x4ca123, 255)) {
			t.Error("full-scale frame rejected at min 0 dBFS")
		}
		if fz.Accept(modeS(17, 0x4ca123, 254)) {
			t.Error("sub-scale frame accepted at min 0 dBFS")
		}
	})
}

func TestFilterRulesCombined(t *testing.T) {
	min := -10.0
	f := mustNew(t, Config{MinRSSI: &min, AllowHex: "4ca123 abc123", DenyHex: "4ca123"})

	cases := []struct {
		name  string
		frame beast.Frame
		want  bool
	}{
		{"deny beats allow", modeS(17, 0x4ca123, 255), false},
		{"allowed and strong", modeS(17, 0xabc123, 255), true},
		{"allowed but too weak", modeS(17, 0xabc123, 64), false},
		{"not on the allow list", modeS(17, 0xdef456, 255), false},
		{"addressless dropped by allow list", modeAC(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.Accept(tc.frame); got != tc.want {
				t.Errorf("Accept = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewReportsBadLists(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"bad allow entry", Config{AllowHex: "not-hex"}},
		{"bad deny entry", Config{DenyHex: "1234567"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("New succeeded, want error")
			}
		})
	}
}
