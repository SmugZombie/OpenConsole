# OpenConsole Architecture

OpenConsole lets someone share a terminal with another person without exposing
SSH, forwarding ports, or having any inbound connectivity at all. The host makes
a single **outbound** connection to a relay; guests reach the terminal through
that relay.

This document describes the whole planned system and marks what exists today.
Terminal sharing works: a host shares a real shell, and guests attach from
another terminal or from a browser. SSH access and TCP forwarding are still to
come.

## The problem

Sharing a terminal usually means one of: opening an SSH port (attack surface,
often impossible behind NAT/CGNAT), setting up a VPN (heavy), or a hosted SaaS
tool (trust, cost, no self-hosting). OpenConsole's answer is a small relay that
both sides dial out to, so neither end needs to be reachable.

## Components

```
  host machine                    relay server                    guests
 ┌──────────────┐               ┌──────────────┐            ┌──────────────┐
 │ openconsole  │               │ openconsole- │            │  browser     │
 │   CLI        │               │   server     │            │  (xterm.js)  │
 │              │               │              │            ├──────────────┤
 │  shell ↔ PTY │──outbound────▶│  sessions    │◀───────────│ openconsole  │
 │  (terminal)  │   WebSocket   │  + bridges   │ WebSocket  │    join      │
 └──────────────┘               │  + web UI    │            ├──────────────┤
                                └──────────────┘            │  ssh (later) │
                                                            └──────────────┘
```

### `cmd/openconsole` — host CLI

Starts a shell on a PTY, asks the relay for a session, dials the tunnel, and
streams terminal bytes. It prints a ticket for whoever is joining and tears the
session down when the shell exits. The same binary joins someone else's terminal
with `openconsole join <ticket>`.

### `cmd/openconsole-server` — relay

Owns sessions and forwards bytes between a host tunnel and its guests. It never
executes anything; it is a broker. Stateless beyond memory, and shipped as a
~10 MB `scratch` container image.

## Package layout and the seams between them

| Package | Responsibility | Status |
| --- | --- | --- |
| `internal/protocol` | Message types, framing, wire encodings. Imports no transport. | ✅ |
| `internal/session` | Identity, credentials, TTL, store, and the live host↔guest bridge. Imports no HTTP and no transport. | ✅ |
| `internal/server` | HTTP API, tunnel endpoint, config, logging, lifecycle. | ✅ |
| `internal/client` | Host CLI: share, join, relay API client. | ✅ |
| `internal/tunnel` | Transport. The only package that knows WebSockets exist. | ✅ |
| `internal/terminal` | PTY and shell handling. Produces and consumes bytes. | ✅ |
| `internal/webui` | Serves the embedded browser client. | ✅ |
| `web/` | Browser client sources: TypeScript, Vite, xterm.js. | ✅ |

The layering rule that everything else follows from: **terminal data is never
coupled to WebSockets.** `internal/terminal` produces and consumes byte streams,
`internal/protocol` describes frames, and `internal/tunnel` is the only package
that imports a WebSocket library. Swapping in QUIC, an SSH channel or a plain
TCP socket touches exactly one package.

The session bridge takes this one step further. Rather than importing
`internal/tunnel` for its connection type, it declares the interface it needs:

```go
// internal/session
type Stream interface {
	Send(ctx context.Context, f protocol.Frame) error
	Recv(ctx context.Context) (protocol.Frame, error)
	Close(reason string) error
}
```

The consumer declares the interface, the transport satisfies it structurally,
and neither package imports the other. Session management therefore has no
transport dependency at all, and the whole fan-out is tested against an
in-memory pipe with no network involved.

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
| `GET` | `/api/v1/tunnel` | WebSocket upgrade. Both roles; the OPEN frame says which. |

Unknown, malformed and expired IDs all return an identical `404
session_not_found`. Distinguishing them would let a caller probe which IDs were
ever valid.

Routing uses the Go 1.22+ `net/http` pattern mux — method matching and path
wildcards are all this service needs.

The tunnel is one endpoint for both roles rather than a path per session, so the
URL is identical for every connection and the access log records nothing
sensitive. Credentials arrive in the `OPEN` frame instead. See
[protocol.md](protocol.md).

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

## The live session bridge

A session record and its live wiring are separate things. A session exists as
soon as it is created over HTTP; it gets a **bridge** only when a host actually
connects a terminal. An abandoned session costs a map entry, not a goroutine.

The bridge is where fan-out happens: one host terminal, any number of guests
watching and typing into it.

```
        ┌──────────────── Bridge ────────────────┐
        │                                        │
host ──▶│  scrollback ring (64 KiB)              │
        │       │                                │
        │       ├──▶ guest queue ──▶ guest A     │
        │       ├──▶ guest queue ──▶ guest B     │
        │       └──▶ guest queue ──▶ guest C     │
        │                                        │
        │◀──────── guest input (merged) ─────────│
        └────────────────────────────────────────┘
```

Three decisions worth naming:

**A slow guest is dropped, never allowed to block.** Each guest has a bounded
outbound queue. A guest that falls behind is disconnected rather than applying
backpressure to the host. Freezing a shared terminal because one viewer is on a
bad connection would be much worse than losing that viewer.

**Teardown has two flavours.** A graceful stop lets a guest's writer flush what
is already queued — that is how the final `CLOSE` reaches a guest when the host
exits, instead of the connection simply vanishing. A hard drop skips the flush
and closes the transport, because a guest gets dropped precisely when it has
stopped reading, and whatever goroutine is writing to it is blocked *inside*
`Send`. Closing the stream is the only thing that unblocks it.

**Joining mid-session replays scrollback.** A guest attaching to a terminal
already in use is sent the host's current size and up to 64 KiB of recent
output, so it has something on screen immediately rather than a blank rectangle.

## Operational behaviour

- **Logging**: `log/slog` JSON to stderr. One line per request with method,
  path, status, duration and remote host. Query strings and headers are not
  logged, because that is where a credential would appear.
- **Timeouts**: 5s read-header, 60s idle. `ReadTimeout` and `WriteTimeout` are
  deliberately unset: they are whole-connection deadlines, and a tunnel carrying
  an idle terminal would trip them and drop a working session. Slow-client
  protection comes from the read-header timeout, the request body cap, and
  protocol-level PING/PONG.
- **Shutdown**: SIGINT/SIGTERM cancels the root context; in-flight requests get
  10 seconds. `http.Server.Shutdown` does not touch hijacked WebSocket
  connections, so tunnels are closed explicitly — peers are told, then the
  context their goroutines are parked on is cancelled.
- **Panic recovery**: a panic in one request must not drop every live session on
  the process, so the handler chain recovers and returns a 500.

One subtlety worth recording, because it cost a debugging session: the logging
middleware wraps `http.ResponseWriter`, and a wrapper that does not implement
`http.Hijacker` breaks WebSocket upgrades with a 501 while leaving the entire
REST API working perfectly. `statusRecorder` passes `Hijack` and `Flush`
through.

## The browser client

`web/` builds to `internal/webui/dist` and is embedded with `go:embed`, so the
relay stays one file with nothing to deploy alongside it. The bundle is
committed, so `go build ./...` produces a working UI without a Node toolchain;
the Docker image rebuilds it from source so a container is never stale.

Routes are registered explicitly — `/{$}`, `/s/{id}`, `/assets/`, and each
root-level file in the bundle — rather than as a catch-all `GET /`. A catch-all
sounds harmless but quietly changes API behaviour: it matches
`GET /api/v1/sessions` and returns the UI's 404 instead of the 405 the pattern
mux gives for a path that exists under a different method.

### The capability URL

A guest link is `/s/<session-id>#<guest-token>`. The token is in the fragment,
which browsers never transmit, so the relay sees only `GET /s/<session-id>` and
the credential never reaches an access log, a proxy log, or a `Referer` header.
The page reads `location.hash` and sends the token in an `OPEN` frame like any
other client. This is the one place the project puts a credential in a URL, and
it is deliberate; see [protocol.md](protocol.md).

The page is served with a strict CSP (everything is same-origin and embedded),
`Referrer-Policy: no-referrer`, `nosniff` and `X-Frame-Options: DENY`.

### Sizing

The host owns its terminal's dimensions. Guests are told the size on join and
whenever it changes, and pick a font size that makes that grid fit their window
— they never resize someone else's real terminal. A `RESIZE` from a guest is
ignored by the relay.

## Deployment

The relay ships as a `scratch` image containing one static binary, running as
uid 65532. No shell, no package manager, no libc. The `HEALTHCHECK` invokes the
binary's own `-healthcheck` flag, which probes `/health` over loopback — the
image has no curl to call.

It terminates no TLS and must sit behind a proxy in any deployment reachable
from outside a trusted network. See [../deploy/README.md](../deploy/README.md)
for proxy configuration; the one requirement people miss is that a proxy which
strips `Upgrade` headers lets the REST API work while every terminal silently
fails to connect.

## Roadmap

| Phase | Scope | Status |
| --- | --- | --- |
| 1 | Sessions, IDs/tokens, HTTP API, config, logging, docs. | ✅ |
| 2 | PTY + shell, WebSocket tunnel, host↔guest streaming, terminal guest client, container image. | ✅ |
| 3 | Browser client: Vite + TypeScript + xterm.js, embedded in the relay. | ✅ |
| 4 | Native SSH access (`ssh <session>@relay`). | next |
| 5 | Multiplexed channels for TCP forwarding; read-only guests. | |
| 6 | End-to-end encryption between host and guest. | |

## Known gaps

1. **No authentication on `POST /api/v1/sessions`.** Anyone who can reach the
   relay can create sessions. Fine for a self-hosted relay on a trusted network;
   a public relay needs at minimum a rate limit and probably a shared secret.
2. **No rate limiting or session cap.** Session creation is unbounded, which is
   a memory-exhaustion vector. The container limits in `docker-compose.yml`
   bound the blast radius but are not a substitute.
3. **Tokens are bearer credentials in cleartext.** TLS is assumed to be
   terminated by a proxy in front of the relay. This is documented in
   `deploy/README.md` but not enforced by the code.
4. **The relay sees plaintext terminal traffic.** Anyone who controls the relay
   can read and inject keystrokes. End-to-end encryption is roadmap, and until
   it exists "self-hosted" is doing real security work.
5. **Guests have full write access.** Anyone with a ticket can type. Read-only
   guests are roadmap.
6. **Single-process only.** Sessions and bridges live in one process's memory,
   so the relay cannot be horizontally scaled without sticky routing.
7. **No Windows host support.** Sharing needs a PTY; ConPTY is not wired up. The
   relay and the join client build and run on Windows, but `openconsole` cannot
   share a terminal there.
8. **A guest link is a bearer capability.** Anyone who obtains the full URL has
   the terminal. It cannot be revoked short of ending the session, and there is
   no per-guest identity or audit trail.
9. **The committed web bundle can go stale.** `internal/webui/dist` is generated
   but checked in so `go build` needs no Node. Nothing yet fails a build when it
   is older than `web/src`; the Docker image sidesteps this by rebuilding, but a
   CI check would be better.
