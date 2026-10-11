# beast-relay

[![CI](https://github.com/technothatch/beast-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/technothatch/beast-relay/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://github.com/technothatch/beast-relay/blob/main/LICENSE)
[![Latest Release](https://img.shields.io/github/v/release/technothatch/beast-relay)](https://github.com/technothatch/beast-relay/releases)
[![Issues](https://img.shields.io/github/issues/technothatch/beast-relay)](https://github.com/technothatch/beast-relay/issues)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](https://github.com/technothatch/beast-relay/pulls)
[![Code size](https://img.shields.io/github/languages/code-size/technothatch/beast-relay)](https://github.com/technothatch/beast-relay)
[![Last commit](https://img.shields.io/github/last-commit/technothatch/beast-relay)](https://github.com/technothatch/beast-relay/commits/main)
[![Go Version](https://img.shields.io/github/go-mod/go-version/technothatch/beast-relay)](https://github.com/technothatch/beast-relay/blob/main/go.mod)
[![Coverage](https://img.shields.io/badge/coverage-93.8%25-brightgreen.svg)](https://github.com/technothatch/beast-relay)

## Overview

**dump1090-fa** and **readsb** each expose exactly *one* binary Beast output —
TCP port 30005 — but a typical station runs several consumers: tar1090, an
MLAT client, a recorder, an analyzer. Only one process can bind a TCP port, so
consumers fight over the feed or get wired together with fragile `socat`
shims. **beast-relay** sits between them: it dials the decoder once
(reconnecting with exponential backoff), accepts N downstream Beast clients,
and forwards every byte exactly as it arrived — optionally filtering by ICAO
address or signal level on the way through.

## Features

- **Byte-preserving fan-out** — frames pass through untouched, 0x1a escaping
  included; with no filter configured the relay does not parse them at all.
- **Slow clients never stall the bus** — each client gets a bounded queue
  (default 1 MiB) with drop-oldest eviction, counted in
  `beast_relay_client_dropped_bytes_total`.
- **ICAO filtering** — `--allow-hex` / `--deny-hex` take full addresses or
  prefixes (`4ca` matches the whole 0x4CAxxx block); deny always wins, and an
  active allow list drops addressless frames (Mode A/C, unsupported DFs).
  Address/Parity replies (DF0/4/5/16/20/21, DF24+) are decoded through the
  Mode S CRC syndrome, the same rule dump1090 uses.
- **Signal-level filtering** — `--filter-min-rssi` drops frames below a dBFS
  threshold (the same units as `rssi` in `aircraft.json`).
- **Resilient upstream** — reconnects with jittered exponential backoff from
  `--reconnect-min` (500ms) to `--reconnect-max` (30s); the decoder can
  restart without touching the relay.
- **Guard rails** — `--max-clients` refuses excess connections,
  `--idle-timeout` reaps clients that hold queued data but stop reading it.
- **Metrics and health** — Prometheus counters plus `/healthz` on
  `--metrics :9703`.
- **Small surface** — stdlib (`net`, `flag`, `log/slog`) plus
  `prometheus/client_golang`; graceful shutdown on SIGINT/SIGTERM.

## Install

```bash
go install github.com/technothatch/beast-relay/cmd/beast-relay@latest
```

Or build from source:

```bash
git clone https://github.com/technothatch/beast-relay.git
cd beast-relay
go build -o beast-relay ./cmd/beast-relay
```

Prebuilt binaries ship on the [releases page](https://github.com/technothatch/beast-relay/releases).

## Quickstart

See every flag with `beast-relay -h` (real output):

```text
Usage of beast-relay:
  -allow-hex string
    	comma/space separated ICAO addresses or prefixes to keep, e.g. 4ca123,abc (implies frame filtering)
  -client-buffer int
    	per-client queue size in bytes; the oldest bytes are dropped when it fills (default 1048576)
  -deny-hex string
    	comma/space separated ICAO addresses or prefixes to drop; deny wins over allow
  -filter-min-rssi float
    	minimum signal level in dBFS to forward (only applied when the flag is set)
  -idle-timeout duration
    	disconnect a client that still has queued data but stops accepting writes (0 disables the check and the write deadline) (default 1m0s)
  -listen string
    	address to serve downstream Beast clients on (default ":30005")
  -max-clients int
    	maximum simultaneous downstream clients (0 = unlimited) (default 32)
  -metrics string
    	address for /metrics and /healthz (empty disables) (default ":9703")
  -reconnect-max duration
    	maximum upstream reconnect backoff (default 30s)
  -reconnect-min duration
    	initial upstream reconnect backoff (default 500ms)
  -upstream string
    	Beast source to relay, host:port (env BEAST_RELAY_UPSTREAM) (default "localhost:30005")
```

Point it at your decoder and every Beast consumer on your network can dial
port 30005 instead:

```bash
beast-relay --upstream dump1090.lan:30005
```

A complete local run — below is genuine output from this machine: a stand-in
TCP feed listens on `127.0.0.1:31005`, beast-relay fans it out on `:30005`
with metrics on `:9703`, and one downstream client connects:

```text
PS> .\beast-relay.exe --upstream 127.0.0.1:31005 --listen :30005 --metrics :9703
time=2026-10-08T18:32:33.538-04:00 level=INFO msg="relay running" upstream=127.0.0.1:31005 listen=[::]:30005 metrics=:9703 max_clients=32 filtering=false
time=2026-10-08T18:32:33.547-04:00 level=INFO msg="upstream connected" component=relay upstream=127.0.0.1:31005
time=2026-10-08T18:32:34.537-04:00 level=INFO msg="client connected" component=relay client=1 remote=127.0.0.1:50550
```

Health probe and metrics excerpt from the same run:

```text
PS> curl.exe -s http://localhost:9703/healthz
ok
upstream: connected
clients: 1

PS> (curl.exe -s http://localhost:9703/metrics) | Select-String -Pattern '^beast_relay_(upstream_connected|upstream_connects_total|clients|clients_accepted_total|client_bytes_total)'
beast_relay_client_bytes_total 0
beast_relay_clients 1
beast_relay_clients_accepted_total 1
beast_relay_clients_rejected_total 0
beast_relay_upstream_connected 1
beast_relay_upstream_connects_total 1
```

Filtering example — keep only one aircraft block and ignore weak signals:

```bash
beast-relay --upstream dump1090.lan:30005 --allow-hex 4ca --filter-min-rssi -15
```

## Configuration reference

| Flag / env | Meaning | Default |
|---|---|---|
| `--upstream` | Beast source to relay, `host:port` (dump1090/readsb output). | `localhost:30005` |
| `BEAST_RELAY_UPSTREAM` | Supplies `--upstream` when the flag is not given; an explicit flag wins. | unset |
| `--listen` | Address to serve downstream Beast clients on. | `:30005` |
| `--metrics` | Address for `/metrics` and `/healthz`; empty disables the HTTP server. | `:9703` |
| `--max-clients` | Maximum simultaneous downstream clients (0 = unlimited). | `32` |
| `--idle-timeout` | Disconnect a client that still has queued data but stops accepting writes; also the per-write deadline (0 disables both). | `60s` |
| `--client-buffer` | Per-client queue size in bytes; the oldest bytes are dropped when it fills. | `1048576` (1 MiB) |
| `--filter-min-rssi` | Minimum signal level in dBFS to forward; applied only when the flag is set. | disabled |
| `--allow-hex` | Comma/space separated ICAO addresses or prefixes (1–6 hex digits) to keep; implies frame filtering. | empty |
| `--deny-hex` | Comma/space separated ICAO addresses or prefixes to drop; deny wins over allow. | empty |
| `--reconnect-min` | Initial upstream reconnect backoff. | `500ms` |
| `--reconnect-max` | Maximum upstream reconnect backoff. | `30s` |

## How it works

```text
dump1090 / readsb                       beast-relay                      N clients
one Beast output      upstream TCP      +---------------+     +---------- tar1090
:30005  ============> reconnect with    | frame splitter|     +---------- mlat-client
        1 byte stream  backoff + jitter | + ICAO/RSSI   |====>| per-client bounded
                                        |   filter      |     | queue, drop-oldest
                                        +---------------+     +---------- recorder
                                                 |
                                          :9703 /metrics /healthz
```

1. **Upstream loop** — one goroutine dials `--upstream`, reconnecting with
   jittered exponential backoff between `--reconnect-min` and
   `--reconnect-max`, and reads the raw byte stream.
2. **Fan-out** — every read is broadcast to all downstream clients. With no
   filter active the bytes are forwarded verbatim; with a filter the stream is
   split into 0x1a-escaped frames just far enough to read message type, DF and
   ICAO, rejected frames are removed, and everything else is re-emitted
   byte-for-byte.
3. **Per-client queues** — each client owns a bounded queue and its own writer
   goroutine. When the queue fills (a consumer stopped reading), the oldest
   bytes are dropped and counted instead of blocking the bus, so one slow
   client can never stall the rest. `--idle-timeout` disconnects a client that
   keeps queuing but never accepts writes.
4. **Metrics and health** — `/metrics` exposes Prometheus counters for
   clients, bytes, drops, upstream connects/reconnects and filtered frames;
   `/healthz` reports `upstream: connected|disconnected` and the client count
   for liveness probes. Both bind on `--metrics :9703` (empty disables).

## Development

No special tooling — raw commands (all CI runs as well):

```bash
gofmt -l .          # must print nothing
go vet ./...
go build ./...
go test ./...       # table-driven tests, stdlib testing only

# coverage, as reported by the badge
go test -count=1 -coverprofile coverage.out -covermode atomic ./...
go tool cover -func coverage.out
```

A `Makefile` wraps the same commands (`make`, `make test`, `make fmt`,
`make vet`, `make cover`, `make clean`) for those who have `make` installed.
CI additionally runs `golangci-lint` and the tests with `-race`; install
[golangci-lint](https://golangci-lint.run/) locally if you want to run that
check before opening a PR. See `CONTRIBUTING.md` for the ground rules.

## Related projects

- [dump1090-fa](https://github.com/flightaware/dump1090) — the FlightAware
  Mode S decoder whose Beast output this proxies.
- [readsb](https://github.com/wiedehopf/readsb) — a lightweight, performant
  decoder (beast-relay's other first-class source).
- [tar1090](https://github.com/wiedehopf/tar1090) — a fast, reactive web
  map for dump1090/readsb aircraft JSON.
- [graphs1090](https://github.com/wiedehopf/graphs1090) — long-term graphing
  for decoder statistics.

## License

MIT — see [LICENSE](LICENSE). Copyright (c) 2026 technothatch.

---

## Related projects

Part of the [technothatch ADS-B toolkit](https://github.com/technothatch/technothatch#readme) - the full index with one-liners for every tool lives there.

- **Run & operate:** [dump1090-stack](https://github.com/technothatch/dump1090-stack) - full Compose stack; [dump1090-healthcheck](https://github.com/technothatch/dump1090-healthcheck) - decoder watchdog; [dump1090-alerts](https://github.com/technothatch/dump1090-alerts) - rule-based alerting
- **Metrics & data:** [dump1090-exporter](https://github.com/technothatch/dump1090-exporter) - Prometheus; [dump1090-influx](https://github.com/technothatch/dump1090-influx) - InfluxDB/Telegraf; [dump1090-grafana](https://github.com/technothatch/dump1090-grafana) - dashboards; [dump1090-recorder](https://github.com/technothatch/dump1090-recorder) - SQLite history; [dump1090-coverage](https://github.com/technothatch/dump1090-coverage) - heatmaps & reports
- **Feeds & networking:** [beast-relay](https://github.com/technothatch/beast-relay) - Beast fan-out; [dump1090-geofence](https://github.com/technothatch/dump1090-geofence) - region-filtered feeds; [dump1090-to-mqtt](https://github.com/technothatch/dump1090-to-mqtt) - MQTT/Home Assistant
- **Interfaces:** [dump1090-dashboard](https://github.com/technothatch/dump1090-dashboard) - web UI; [adsb-tui](https://github.com/technothatch/adsb-tui) - terminal radar; [dump1090-cli](https://github.com/technothatch/dump1090-cli) - aircraft.json CLI; [dump1090-sdk](https://github.com/technothatch/dump1090-sdk) - TypeScript client
- **Testing & decoding:** [dump1090-simulator](https://github.com/technothatch/dump1090-simulator) - synthetic traffic; [dump1090-replay](https://github.com/technothatch/dump1090-replay) - recorded history replay; [squawk-decoder](https://github.com/technothatch/squawk-decoder) - squawk/emitter/ICAO decoder
