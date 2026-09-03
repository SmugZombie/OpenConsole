# OpenConsole Architecture

OpenConsole lets someone share a terminal with another person without exposing
SSH, forwarding ports, or having any inbound connectivity at all. The host makes
a single **outbound** connection to a relay; guests reach the terminal through
that relay.

This document describes the whole planned system and marks what exists today.
Phase 1 implements session management and the HTTP API only.

## The problem

Sharing a terminal usually means one of: opening an SSH port (attack surface,
often impossible behind NAT/CGNAT), setting up a VPN (heavy), or a hosted SaaS
tool (trust, cost, no self-hosting). OpenConsole's answer is a small relay that
both sides dial out to, so neither end needs to be reachable.

## Components

```
  host machine                    relay server                    guest
 ┌──────────────┐               ┌──────────────┐            ┌──────────────┐
 │ openconsole  │               │ openconsole- │            │  browser     │
 │   CLI        │               │   server     │            │  (xterm.js)  │
 │              │               │              │            │              │
 │  shell ↔ PTY │──outbound────▶│  sessions    │◀───────────│  or ssh      │
 │  (terminal)  │   WebSocket   │  + tunnels   │ WebSocket  │              │
 └──────────────┘               └──────────────┘            └──────────────┘
```

### `cmd/openconsole` — host CLI

Will start or attach to a local shell on a PTY, ask a relay for a session, dial
the relay's tunnel endpoint, and stream terminal bytes. It prints the join URL
and tears the session down when the shell exits.

*Today:* configuration parsing (`-server`, `OPENCONSOLE_SERVER`) and version
output.

### `cmd/openconsole-server` — relay

Owns sessions and forwards bytes between a host tunnel and its guests. It never
executes anything; it is a broker. Deliberately stateless beyond memory and
deployable as a single binary or container.

*Today:* the session HTTP API, structured logging, graceful shutdown.

## Package layout and the seams between them

| Package | Responsibility | Status |
| --- | --- | --- |
| `internal/protocol` | Message types and framing rules. Imports no transport. | Phase 1 |
| `internal/session` | Session identity, credentials, TTL, in-memory store. Imports no HTTP. | Phase 1 |
| `internal/server` | HTTP API, config, logging, lifecycle. | Phase 1 |
| `internal/client` | Host CLI logic and config. | Phase 1 (config only) |
| `internal/tunnel` | Transport: carries protocol frames over a connection. | Phase 2 |
| `internal/terminal` | PTY and shell handling. | Phase 2 |

The layering rule that everything else follows from: **terminal data is never
coupled to WebSockets.** `internal/terminal` will produce and consume byte
streams, `internal/protocol` will describe frames, and `internal/tunnel` will be
the only package that knows a WebSocket exists. Swapping in QUIC, an SSH channel
or a plain TCP socket should touch exactly one package.

`internal/tunnel` and `internal/terminal` do not exist yet. Creating them empty
now would be an abstraction with nothing behind it; they arrive with their first
real implementation.

## Session model

A session carries three distinct values, and the distinction is the security
boundary:

| Field | Secret? | Purpose |
| --- | --- | --- |
| `SessionID` | No | Public handle. Appears in URLs, logs and eventually as an SSH username. 128 bits from `crypto/rand`. |
| `HostToken` | **Yes** | Authenticates the host's tunnel connection. 256 bits. |
| `GuestToken` | **Yes** | Authenticates a guest joining. 256 bits. |

Both tokens are returned exactly once, in the `POST /api/v1/sessions` response.
No other endpoint returns them and nothing logs them. Keeping the public ID
separate from the credentials means a session ID can be shown, quoted in a
support ticket, or logged by a proxy without granting access.

Storage is in-memory with a TTL (default 30 minutes), swept in the background
and enforced on every lookup. Persistence would buy nothing yet: a session is
only meaningful while the relay process holding its live tunnel is running.

## HTTP API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/health` | Liveness, version, live session count. |
| `POST` | `/api/v1/sessions` | Create a session. **Only** response containing tokens. |
| `GET` | `/api/v1/sessions/{id}` | Public session metadata. No credentials. |

Unknown, malformed and expired IDs all return an identical `404
session_not_found`. Distinguishing them would let a caller probe which IDs were
ever valid.

Routing uses the Go 1.22+ `net/http` pattern mux — method matching and path
wildcards are all this service needs, and it keeps the dependency list empty.

## Configuration

Precedence is defaults → environment → flags, so an operator can override a
container's environment for a one-off run.

| Setting | Env | Flag | Default |
| --- | --- | --- | --- |
| Listen address | `OPENCONSOLE_LISTEN_ADDR` | `-listen` | `:8080` |
| Session TTL | `OPENCONSOLE_SESSION_TTL` | `-session-ttl` | `30m` |
| Log level | `OPENCONSOLE_LOG_LEVEL` | `-log-level` | `info` |

Durations must be Go duration strings (`30m`, `1h30m`). A bare `30` is rejected
rather than guessed at.

## Operational behaviour

- **Logging**: `log/slog` JSON to stderr. One line per request with method,
  path, status, duration and remote host. Query strings and headers are not
  logged, because that is where a credential would appear.
- **Timeouts**: 5s read-header, 15s read/write, 60s idle. These will need
  per-route relaxation once long-lived tunnels exist — see below.
- **Shutdown**: SIGINT/SIGTERM cancels the root context; in-flight requests get
  10 seconds. A second signal restores default behaviour so the process can
  always be forced down.
- **Panic recovery**: a panic in one request must not drop every live session on
  the process, so the handler chain recovers and returns a 500.

## Roadmap

| Phase | Scope |
| --- | --- |
| 1 ✅ | Sessions, IDs/tokens, HTTP API, config, logging, docs. |
| 2 | PTY + shell (`internal/terminal`), WebSocket tunnel (`internal/tunnel`), host↔relay streaming. |
| 3 | Browser client: Vite + TypeScript + xterm.js under `web/`. |
| 4 | Docker image and `deploy/` compose files. |
| 5 | Native SSH access (`ssh <session>@relay`). |
| 6 | Multiplexed channels for TCP forwarding; read-only guests; E2E encryption. |

## Known gaps to settle before Phase 2

1. **No authentication on `POST /api/v1/sessions`.** Anyone who can reach the
   relay can create sessions. Fine for a self-hosted relay on a trusted network;
   a public relay needs at minimum a rate limit and probably a shared secret.
2. **No rate limiting or session cap.** Session creation is unbounded, which is
   a memory-exhaustion vector.
3. **Write/idle timeouts are wrong for tunnels.** A WebSocket carrying an idle
   terminal will exceed them. The tunnel routes will need their own server or
   per-connection deadline handling driven by PING/PONG.
4. **Tokens are bearer credentials in cleartext.** TLS is assumed to be
   terminated by the relay or a proxy in front of it; this is not enforced or
   documented as a deployment requirement yet.
5. **Single-process only.** Sessions live in one process's memory, so the relay
   cannot be horizontally scaled without sticky routing or shared state.
