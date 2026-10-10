// Package filter decides which Beast frames a relay forwards downstream.
//
// Three independent rules can be active at once:
//
//   - MinRSSI drops frames whose signal level is below a dBFS threshold
//     (the same units as the rssi field of aircraft.json).
//   - DenyHex drops frames from listed ICAO addresses; deny always wins
//     over allow.
//   - AllowHex keeps only frames from listed ICAO addresses. When an allow
//     list is active, a frame with no decodable address (Mode A/C, an
//     unsupported DF) is dropped, so a list cannot leak traffic it does not
//     name. Deny lists never block such frames, because there is no address
//     to accuse.
//
// Hex entries are 1-6 hex digits; an entry shorter than 6 digits matches an
// address prefix (e.g. "4ca" matches every aircraft in the 0x4CAxxx block).
package filter

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/technothatch/beast-relay/internal/beast"
)

// Config carries the raw flag values; New parses and validates them.
type Config struct {
	// MinRSSI is the minimum signal level in dBFS to forward; nil disables
	// the rule.
	MinRSSI *float64
	// AllowHex is a comma/space separated list of ICAO addresses (or
	// prefixes) to keep. Empty disables the rule.
	AllowHex string
	// DenyHex is a comma/space separated list of ICAO addresses (or
	// prefixes) to drop. Empty disables the rule.
	DenyHex string
}

// rule is one parsed address: value is left-aligned within the 24-bit
// address and mask covers its significant digits.
type rule struct {
	value uint32
	mask  uint32
}

// Filter is an immutable, concurrency-safe set of accept rules.
type Filter struct {
	minRSSI *float64
	allow   []rule
	deny    []rule
}

// New parses the config and builds a Filter.
func New(cfg Config) (*Filter, error) {
	allow, err := parseHexList(cfg.AllowHex)
	if err != nil {
		return nil, fmt.Errorf("allow-hex: %w", err)
	}
	deny, err := parseHexList(cfg.DenyHex)
	if err != nil {
		return nil, fmt.Errorf("deny-hex: %w", err)
	}
	return &Filter{minRSSI: cfg.MinRSSI, allow: allow, deny: deny}, nil
}

// Active reports whether any rule would reject a frame. The relay uses it
// to choose between parsing frames and forwarding bytes untouched.
func (f *Filter) Active() bool {
	return f != nil && (f.minRSSI != nil || len(f.allow) > 0 || len(f.deny) > 0)
}

// Accept reports whether a frame may be forwarded downstream.
func (f *Filter) Accept(fr beast.Frame) bool {
	if f == nil {
		return true
	}
	if f.minRSSI != nil && fr.RSSI() < *f.minRSSI {
		return false
	}
	icao, ok := fr.ICAO()
	if !ok {
		return len(f.allow) == 0
	}
	if matches(f.deny, icao) {
		return false
	}
	if len(f.allow) > 0 && !matches(f.allow, icao) {
		return false
	}
	return true
}

// matches reports whether any rule covers the address.
func matches(rules []rule, icao uint32) bool {
	for _, r := range rules {
		if icao&r.mask == r.value {
			return true
		}
	}
	return false
}

// parseHexList splits a separator-tolerant list of hex addresses and
// parses each entry into a rule.
func parseHexList(s string) ([]rule, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
	rules := make([]rule, 0, len(fields))
	for _, field := range fields {
		r, err := parseRule(field)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// parseRule accepts 1-6 hex digits with an optional 0x prefix.
func parseRule(s string) (rule, error) {
	digits := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if n := len(digits); n < 1 || n > 6 {
		return rule{}, fmt.Errorf("%q: want 1-6 hex digits", s)
	}
	v, err := strconv.ParseUint(digits, 16, 24)
	if err != nil {
		return rule{}, fmt.Errorf("%q: not a hex address", s)
	}
	shift := uint(4 * (6 - len(digits)))
	value := uint32(v) << shift
	// Both value and mask are left-aligned within the 24-bit address, so
	// the mask keeps the prefix's high bits and zeroes the free low bits.
	mask := (uint32(0xffffff) >> shift) << shift
	return rule{value: value, mask: mask}, nil
}
