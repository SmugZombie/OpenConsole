# OpenConsole

Share a terminal with someone, temporarily, without exposing SSH, forwarding a
port, or having any inbound connectivity.

OpenConsole is an open-source, self-hostable take on the idea behind the
discontinued [Teleconsole](https://github.com/gravitational/teleconsole). It is a
new implementation, not a fork.

The host runs one command. It dials **out** to a relay you control, and prints a
temporary link. Someone else opens that link and shares the terminal. When the
host exits, the session is gone.

> **Status: Phase 1 — early scaffolding.**
> Session management and the relay's HTTP API work today. There is no terminal
> sharing yet: no PTY, no WebSocket tunnel, no browser UI. See
> [the roadmap](docs/architecture.md#roadmap).

## How it will work

```
  host machine                    relay server                    guest
 ┌──────────────┐               ┌──────────────┐            ┌──────────────┐
 │ openconsole  │──outbound────▶│  sessions    │◀───────────│  browser     │
 │  shell + PTY │   WebSocket   │  + tunnels   │ WebSocket  │  (xterm.js)  │
 └──────────────┘               └──────────────┘            └──────────────┘
```

Neither end needs to be reachable from the internet. The relay never executes
anything — it only brokers bytes.

## Requirements

- Go 1.22 or newer (`go1.22` is the module's minimum; developed on 1.27)
- No other dependencies. The module has zero third-party requirements.

## Quick start

```sh
git clone https://github.com/SmugZombie/OpenConsole.git
cd OpenConsole
go build ./...
```

### Run the relay

```sh
go run ./cmd/openconsole-server
```

```
{"time":"...","level":"INFO","msg":"relay listening","addr":"[::]:8080","session_ttl":"30m0s"}
```

Or build a binary:

```sh
go build -o bin/openconsole-server ./cmd/openconsole-server
./bin/openconsole-server -listen 127.0.0.1:8080 -session-ttl 15m -log-level debug
```

### Run the CLI

```sh
go run ./cmd/openconsole
go run ./cmd/openconsole -h
go run ./cmd/openconsole -version
```

Phase 1 prints its version and the relay it would use. Terminal sharing is not
wired up yet.

## Try the API

With the relay running on `:8080`:

```sh
# Health
curl -s localhost:8080/health

# Create a session — this is the ONLY response that contains tokens
curl -s -X POST localhost:8080/api/v1/sessions

# Look it up (no credentials in this response)
ID=$(curl -s -X POST localhost:8080/api/v1/sessions | \
     python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')
curl -s localhost:8080/api/v1/sessions/$ID

# Unknown, malformed and expired IDs all answer the same 404
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/api/v1/sessions/nope
```

`POST /api/v1/sessions` responds:

```json
{
  "session_id": "yrkw3xkrbtqsuu4jbxjqquefpu",
  "host_token": "…",
  "guest_token": "…",
  "created_at": "2026-09-03T18:00:00Z",
  "expires_at": "2026-09-03T18:30:00Z",
  "expires_in_seconds": 1800
}
```

Treat `host_token` and `guest_token` as secrets — they are shown once, and no
other endpoint or log line will ever repeat them.

## Configuration

Precedence is **defaults → environment → flags**.

### Relay (`openconsole-server`)

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-listen` | `OPENCONSOLE_LISTEN_ADDR` | `:8080` | Listen address |
| `-session-ttl` | `OPENCONSOLE_SESSION_TTL` | `30m` | Session lifetime |
| `-log-level` | `OPENCONSOLE_LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` |

Durations are Go duration strings (`30m`, `1h30m`). A bare `30` is rejected
rather than guessed at.

### CLI (`openconsole`)

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-server` | `OPENCONSOLE_SERVER` | `http://localhost:8080` | Relay base URL |
| `-version` | — | — | Print version and exit |

## Development

```sh
gofmt -l .          # must print nothing
go vet ./...
go test ./...
go test -race ./... # the session manager is concurrent; keep this green
```

Build with a version stamp:

```sh
go build -ldflags "-X main.version=v0.1.0" -o bin/openconsole-server ./cmd/openconsole-server
go build -ldflags "-X main.version=v0.1.0" -o bin/openconsole ./cmd/openconsole
```

### Layout

```
cmd/openconsole/          host CLI (skeleton)
cmd/openconsole-server/   relay server
internal/protocol/        message types; imports no transport
internal/session/         IDs, tokens, TTL, in-memory store; imports no HTTP
internal/server/          HTTP API, config, logging, lifecycle
internal/client/          CLI config
docs/                     architecture.md, protocol.md
```

`internal/tunnel/` and `internal/terminal/` are planned but not created — an
empty package is an abstraction with nothing behind it. They arrive with their
first real implementation.

## Design notes

- **Terminal data is never coupled to WebSockets.** `internal/protocol`
  describes frames; only the (future) tunnel package will know a transport.
  See [docs/protocol.md](docs/protocol.md).
- **Terminal bytes stay binary.** `DATA` frames are raw — no JSON, no base64.
  Control messages are JSON, where being self-describing is worth more than the
  bytes.
- **Public identifiers are separate from credentials.** `session_id` is safe to
  log and print; `host_token`/`guest_token` are not, and nothing in the codebase
  logs them.
- **All identifiers come from `crypto/rand`.** 128 bits for session IDs, 256 for
  tokens. `math/rand` is never used.
- **Zero dependencies.** Routing is the Go 1.22 `net/http` pattern mux; logging
  is `log/slog`.

## Security

Not yet suitable for exposure to the public internet. In particular, session
creation is unauthenticated and unrate-limited, and TLS is assumed to be
terminated in front of the relay. The full list of gaps is tracked in
[docs/architecture.md](docs/architecture.md#known-gaps-to-settle-before-phase-2).

## License

Not yet chosen.
