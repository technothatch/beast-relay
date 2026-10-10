package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"testing"
	"time"
)

func TestParseOptionsDefaults(t *testing.T) {
	o, err := parseOptions(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.upstream != defaultUpstream {
		t.Errorf("upstream = %q, want %q", o.upstream, defaultUpstream)
	}
	if o.listen != ":30005" {
		t.Errorf("listen = %q, want :30005", o.listen)
	}
	if o.metricsAddr != ":9703" {
		t.Errorf("metrics = %q, want :9703", o.metricsAddr)
	}
	if o.maxClients != 32 {
		t.Errorf("maxClients = %d, want 32", o.maxClients)
	}
	if o.idleTimeout != 60*time.Second {
		t.Errorf("idleTimeout = %s, want 60s", o.idleTimeout)
	}
	if o.clientBuffer != 1<<20 {
		t.Errorf("clientBuffer = %d, want %d", o.clientBuffer, 1<<20)
	}
	if o.minRSSI != nil {
		t.Errorf("minRSSI = %v, want nil (no RSSI filter by default)", *o.minRSSI)
	}
	if o.reconnectMin != 500*time.Millisecond || o.reconnectMax != 30*time.Second {
		t.Errorf("backoff = [%s, %s], want [500ms, 30s]", o.reconnectMin, o.reconnectMax)
	}
	if o.allowHex != "" || o.denyHex != "" {
		t.Errorf("filters = (%q, %q), want empty", o.allowHex, o.denyHex)
	}
}

func TestParseOptionsOverrides(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		check func(t *testing.T, o *options)
	}{
		{
			name: "all flags",
			args: []string{
				"--upstream", "10.0.0.2:30005",
				"--listen", "127.0.0.1:31005",
				"--metrics", "",
				"--max-clients", "4",
				"--idle-timeout", "5s",
				"--client-buffer", "4096",
				"--filter-min-rssi", "-15.5",
				"--allow-hex", "4ca123,abc123",
				"--deny-hex", "def456",
				"--reconnect-min", "1s",
				"--reconnect-max", "10s",
			},
			check: func(t *testing.T, o *options) {
				if o.upstream != "10.0.0.2:30005" {
					t.Errorf("upstream = %q", o.upstream)
				}
				if o.listen != "127.0.0.1:31005" {
					t.Errorf("listen = %q", o.listen)
				}
				if o.metricsAddr != "" {
					t.Errorf("metrics = %q, want empty (disabled)", o.metricsAddr)
				}
				if o.maxClients != 4 || o.idleTimeout != 5*time.Second || o.clientBuffer != 4096 {
					t.Errorf("numbers = (%d, %s, %d)", o.maxClients, o.idleTimeout, o.clientBuffer)
				}
				if o.minRSSI == nil || *o.minRSSI != -15.5 {
					t.Errorf("minRSSI = %v, want -15.5", o.minRSSI)
				}
				if o.allowHex != "4ca123,abc123" || o.denyHex != "def456" {
					t.Errorf("hex lists = (%q, %q)", o.allowHex, o.denyHex)
				}
				if o.reconnectMin != time.Second || o.reconnectMax != 10*time.Second {
					t.Errorf("backoff = [%s, %s]", o.reconnectMin, o.reconnectMax)
				}
			},
		},
		{
			name: "single upstream flag",
			args: []string{"--upstream", "decoder.lan:30005"},
			check: func(t *testing.T, o *options) {
				if o.upstream != "decoder.lan:30005" {
					t.Errorf("upstream = %q", o.upstream)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseOptions(tc.args, io.Discard)
			if err != nil {
				t.Fatalf("parseOptions: %v", err)
			}
			tc.check(t, o)
		})
	}
}

func TestParseOptionsUpstreamEnvFallback(t *testing.T) {
	t.Setenv(envUpstream, "192.0.2.10:30005")

	o, err := parseOptions(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.upstream != "192.0.2.10:30005" {
		t.Errorf("upstream = %q, want the environment value", o.upstream)
	}

	// An explicit flag still wins over the environment.
	o, err = parseOptions([]string{"--upstream", "198.51.100.7:30005"}, io.Discard)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.upstream != "198.51.100.7:30005" {
		t.Errorf("upstream = %q, want the flag value", o.upstream)
	}
}

func TestParseOptionsMinRSSIOnlyWhenSet(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSet bool
		want    float64
	}{
		{name: "unset stays disabled", args: nil, wantSet: false},
		{name: "negative threshold", args: []string{"--filter-min-rssi", "-15"}, wantSet: true, want: -15},
		{name: "explicit zero applies", args: []string{"--filter-min-rssi", "0"}, wantSet: true, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseOptions(tc.args, io.Discard)
			if err != nil {
				t.Fatalf("parseOptions: %v", err)
			}
			if tc.wantSet {
				if o.minRSSI == nil || *o.minRSSI != tc.want {
					t.Errorf("minRSSI = %v, want %v", o.minRSSI, tc.want)
				}
				return
			}
			if o.minRSSI != nil {
				t.Errorf("minRSSI = %v, want nil", *o.minRSSI)
			}
		})
	}
}

func TestParseOptionsRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"upstream without port", []string{"--upstream", "dump1090"}},
		{"empty upstream", []string{"--upstream", ""}},
		{"empty listen", []string{"--listen", ""}},
		{"negative max clients", []string{"--max-clients", "-1"}},
		{"zero client buffer", []string{"--client-buffer", "0"}},
		{"negative client buffer", []string{"--client-buffer", "-100"}},
		{"negative idle timeout", []string{"--idle-timeout", "-1s"}},
		{"zero reconnect min", []string{"--reconnect-min", "0s"}},
		{"inverted backoff", []string{"--reconnect-min", "2s", "--reconnect-max", "1s"}},
		{"unknown flag", []string{"--bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseOptions(tc.args, io.Discard); err == nil {
				t.Fatal("parseOptions succeeded, want error")
			}
		})
	}
}

func TestParseOptionsHelpIsNotAnError(t *testing.T) {
	_, err := parseOptions([]string{"-h"}, io.Discard)
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("err = %v, want flag.ErrHelp", err)
	}
}

func TestRunExitCodes(t *testing.T) {
	t.Run("invalid flag is exit 2", func(t *testing.T) {
		if code := run(context.Background(), []string{"--max-clients", "-1"}); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
	})
	t.Run("invalid filter is exit 2", func(t *testing.T) {
		if code := run(context.Background(), []string{"--allow-hex", "zzz"}); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
	})
	t.Run("help is exit 0", func(t *testing.T) {
		if code := run(context.Background(), []string{"-h"}); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	t.Run("metrics bind failure is exit 1", func(t *testing.T) {
		args := []string{"--listen", "127.0.0.1:0", "--metrics", "127.0.0.1:99999", "--upstream", "127.0.0.1:1"}
		if code := run(context.Background(), args); code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
	})
	t.Run("cancelled context shuts down cleanly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		args := []string{"--listen", "127.0.0.1:0", "--metrics", "127.0.0.1:0", "--upstream", "127.0.0.1:1"}
		if code := run(ctx, args); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	t.Run("metrics disabled still shuts down cleanly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		args := []string{"--listen", "127.0.0.1:0", "--metrics", "", "--upstream", "127.0.0.1:1"}
		if code := run(ctx, args); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	})
}
