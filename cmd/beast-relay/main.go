// Command beast-relay fans one Beast binary stream out to many consumers.
//
// dump1090-fa and readsb expose a single Beast output on TCP port 30005;
// tar1090, mlat-client, recorders and analyzers each want their own copy.
// beast-relay sits in the middle: it dials the decoder (reconnecting with
// exponential backoff), accepts N downstream clients, and forwards every
// byte untouched — unless --filter-min-rssi, --allow-hex or --deny-hex ask
// it to drop frames, which it can do because it parses the 0x1a-escaped
// frames just far enough to read message type, DF and ICAO.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/technothatch/beast-relay/internal/filter"
	"github.com/technothatch/beast-relay/internal/metrics"
	"github.com/technothatch/beast-relay/internal/relay"
)

const (
	// defaultUpstream is the dump1090/readsb Beast output on localhost.
	defaultUpstream = "localhost:30005"
	// envUpstream overrides --upstream when the flag is not given.
	envUpstream = "BEAST_RELAY_UPSTREAM"
)

// options is the fully resolved flag set.
type options struct {
	upstream     string
	listen       string
	metricsAddr  string
	maxClients   int
	idleTimeout  time.Duration
	clientBuffer int
	reconnectMin time.Duration
	reconnectMax time.Duration
	minRSSI      *float64
	allowHex     string
	denyHex      string
}

// parseOptions resolves flags, the upstream environment fallback and
// validation. flag.Parse errors (including --help, which prints usage)
// are returned unwrapped for run to map onto an exit code.
func parseOptions(args []string, stderr io.Writer) (*options, error) {
	fs := flag.NewFlagSet("beast-relay", flag.ContinueOnError)
	fs.SetOutput(stderr)

	o := &options{}
	upstream := fs.String("upstream", defaultUpstream, "Beast source to relay, host:port (env "+envUpstream+")")
	listen := fs.String("listen", ":30005", "address to serve downstream Beast clients on")
	metricsAddr := fs.String("metrics", ":9703", "address for /metrics and /healthz (empty disables)")
	fs.IntVar(&o.maxClients, "max-clients", 32, "maximum simultaneous downstream clients (0 = unlimited)")
	fs.DurationVar(&o.idleTimeout, "idle-timeout", 60*time.Second, "disconnect a client that still has queued data but stops accepting writes (0 disables the check and the write deadline)")
	fs.IntVar(&o.clientBuffer, "client-buffer", 1<<20, "per-client queue size in bytes; the oldest bytes are dropped when it fills")
	minRSSI := fs.Float64("filter-min-rssi", 0, "minimum signal level in dBFS to forward (only applied when the flag is set)")
	fs.StringVar(&o.allowHex, "allow-hex", "", "comma/space separated ICAO addresses or prefixes to keep, e.g. 4ca123,abc (implies frame filtering)")
	fs.StringVar(&o.denyHex, "deny-hex", "", "comma/space separated ICAO addresses or prefixes to drop; deny wins over allow")
	fs.DurationVar(&o.reconnectMin, "reconnect-min", 500*time.Millisecond, "initial upstream reconnect backoff")
	fs.DurationVar(&o.reconnectMax, "reconnect-max", 30*time.Second, "maximum upstream reconnect backoff")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	o.upstream = strings.TrimSpace(*upstream)
	if !set["upstream"] {
		if v := strings.TrimSpace(os.Getenv(envUpstream)); v != "" {
			o.upstream = v
		}
	}
	// The RSSI filter only exists when explicitly requested: the zero flag
	// value would otherwise silently mean "drop everything at 0 dBFS".
	if set["filter-min-rssi"] {
		o.minRSSI = minRSSI
	}
	o.listen = *listen
	o.metricsAddr = *metricsAddr

	if o.upstream == "" {
		return nil, errors.New("--upstream must not be empty")
	}
	if _, _, err := net.SplitHostPort(o.upstream); err != nil {
		return nil, fmt.Errorf("--upstream %q must be host:port: %w", o.upstream, err)
	}
	if o.listen == "" {
		return nil, errors.New("--listen must not be empty")
	}
	if o.maxClients < 0 {
		return nil, fmt.Errorf("--max-clients must be >= 0, got %d", o.maxClients)
	}
	if o.clientBuffer <= 0 {
		return nil, fmt.Errorf("--client-buffer must be positive, got %d", o.clientBuffer)
	}
	if o.idleTimeout < 0 {
		return nil, fmt.Errorf("--idle-timeout must be >= 0, got %s", o.idleTimeout)
	}
	if o.reconnectMin <= 0 {
		return nil, fmt.Errorf("--reconnect-min must be positive, got %s", o.reconnectMin)
	}
	if o.reconnectMin > o.reconnectMax {
		return nil, fmt.Errorf("--reconnect-min %s exceeds --reconnect-max %s", o.reconnectMin, o.reconnectMax)
	}
	return o, nil
}

// run wires the pieces together and blocks until ctx is cancelled (SIGINT
// or SIGTERM, wired up in main), then shuts down gracefully. It returns the
// exit code for main.
func run(ctx context.Context, args []string) int {
	opts, err := parseOptions(args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "beast-relay:", err)
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	flt, err := filter.New(filter.Config{
		MinRSSI:  opts.minRSSI,
		AllowHex: opts.allowHex,
		DenyHex:  opts.denyHex,
	})
	if err != nil {
		logger.Error("invalid filter", "error", err)
		return 2
	}

	m := metrics.New()
	rel, err := relay.New(relay.Config{
		Upstream:     opts.upstream,
		Listen:       opts.listen,
		MaxClients:   opts.maxClients,
		IdleTimeout:  opts.idleTimeout,
		ClientBuffer: opts.clientBuffer,
		ReconnectMin: opts.reconnectMin,
		ReconnectMax: opts.reconnectMax,
		Filter:       flt,
		Metrics:      m,
		Logger:       logger,
	})
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return 2
	}
	if err := rel.Start(ctx); err != nil {
		logger.Error("start relay", "error", err)
		return 1
	}

	// Bind the metrics port synchronously so a typo in --metrics is an
	// immediate error instead of a silent monitoring outage.
	var metricsSrv *http.Server
	if opts.metricsAddr != "" {
		ln, err := net.Listen("tcp", opts.metricsAddr)
		if err != nil {
			logger.Error("metrics listen", "address", opts.metricsAddr, "error", err)
			_ = rel.Close()
			return 1
		}
		metricsSrv = &http.Server{
			Addr:              opts.metricsAddr,
			Handler:           metrics.Handler(m, rel.Status),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := metricsSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics server", "error", err)
			}
		}()
	}

	logger.Info("relay running",
		"upstream", opts.upstream,
		"listen", rel.Addr().String(),
		"metrics", opts.metricsAddr,
		"max_clients", opts.maxClients,
		"filtering", flt.Active(),
	)

	<-ctx.Done()
	logger.Info("shutting down")
	_ = rel.Close()
	if metricsSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("metrics shutdown timed out", "error", err)
		}
	}
	return 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
