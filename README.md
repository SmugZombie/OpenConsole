# OpenConsole

Share a terminal with someone, temporarily, without exposing SSH, forwarding a
port, or having any inbound connectivity.

OpenConsole is an open-source, self-hostable take on the idea behind the
discontinued [Teleconsole](https://github.com/gravitational/teleconsole). It is a
new implementation, not a fork.

You run one command. It dials **out** to a relay you control and prints a
ticket. You send the ticket to someone; they run one command and are in your
terminal. When you exit, the session is gone.

```
$ openconsole

openconsole: sharing this terminal
  relay:    https://console.example.com
  session:  x5s5gzxptgfksy3hu75jmcoltm
  expires:  in 30m

  in a browser:
    https://console.example.com/s/x5s5gzxptgfksy3hu75jmcoltm#3u2avt7nibb2oxbz…

  in a terminal:
    openconsole join x5s5gzxptgfksy3hu75jmcoltm.3u2avt7nibb2oxbz…

  The ticket grants full control of this terminal. Send it privately,
  and type 'exit' here to end the session.

$ ▏
```

Send the link, and they are in your terminal — no client to install, nothing to
sign up for.

> **Status: Phase 3.** Terminal sharing works end to end, from another terminal
> or a browser. SSH access and TCP forwarding are not built yet. Session
> creation is unauthenticated, so run your relay on a trusted network or behind
> an authenticating proxy. See [the roadmap](docs/architecture.md#roadmap).

## How it works

```
  host machine                    relay server                    guests
 ┌──────────────┐               ┌──────────────┐            ┌──────────────┐
 │ openconsole  │──outbound────▶│  sessions    │◀───────────│   browser    │
 │  shell + PTY │   WebSocket   │  + bridges   │ WebSocket  │      or      │
 └──────────────┘               │  + web UI    │            │ openconsole  │
                                └──────────────┘            │     join     │
                                                            └──────────────┘
```

Neither end needs to be reachable from the internet — both dial out. The relay
never executes anything; it brokers bytes between one host terminal and any
number of guests.

## Requirements

- Go 1.23 or newer
- macOS or Linux to *share* a terminal (it needs a PTY). The relay and the join
  client also run on Windows.
- Node 20+ only if you want to change the browser client. The built bundle is
  committed, so `go build` alone produces a relay with a working UI.

Four dependencies, all of them small: `coder/websocket`, `creack/pty`,
`golang.org/x/term`, and `golang.org/x/sys`.

## Quick start

Three terminals, all on one machine:

```sh
git clone https://github.com/SmugZombie/OpenConsole.git
cd OpenConsole
go build -o bin/ ./cmd/...
```

**1 — run a relay**

```sh
./bin/openconsole-server
```

**2 — share a terminal**

```sh
./bin/openconsole
```

It prints a ticket. Copy it.

**3 — join**

Open the printed link in a browser, or from another terminal:

```sh
./bin/openconsole join <ticket>
```

Both are now the same shell. Type in either. Press **Ctrl-]** to
detach as a guest; type `exit` as the host to end the session for everyone.

Pointing at a relay somewhere else:

```sh
openconsole -server https://console.example.com
openconsole join <ticket> -server https://console.example.com
# or: export OPENCONSOLE_SERVER=https://console.example.com
```

## Running a relay with Docker

```sh
docker build -t openconsole-server .
docker run --rm -p 8080:8080 openconsole-server
```

Or with compose:

```sh
docker compose -f deploy/docker-compose.yml up -d --build
```

The image is `scratch` plus one static binary — about 10 MB, running as uid
65532, with no shell and a read-only root filesystem. Its `HEALTHCHECK` calls
the binary's own `-healthcheck` flag, so nothing extra is baked in to support
it.

**Behind a proxy**, two things matter: forward WebSocket `Upgrade` headers on
`/api/v1/tunnel`, and do not set a short read timeout on it — an idle terminal
sends nothing for minutes at a time. A proxy that strips `Upgrade` lets the REST
API work perfectly while every terminal silently fails to connect. Worked Caddy
and nginx configs are in [deploy/README.md](deploy/README.md).

## The API

The relay's REST surface, if you want to drive it yourself:

```sh
curl -s localhost:8080/health
# {"status":"ok","version":"dev","sessions":0,"tunnels":0}

curl -s -X POST localhost:8080/api/v1/sessions
# {"session_id":"x5s5…","host_token":"…","guest_token":"…",
#  "created_at":"…","expires_at":"…","expires_in_seconds":1800}

curl -s localhost:8080/api/v1/sessions/x5s5…
# {"session_id":"x5s5…","created_at":"…","expires_at":"…","expires_in_seconds":1799}
```

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/health` | Liveness, version, session and tunnel counts |
| `POST` | `/api/v1/sessions` | Create a session — the only response with tokens |
| `GET` | `/api/v1/sessions/{id}` | Public metadata, no credentials |
| `GET` | `/api/v1/tunnel` | WebSocket upgrade for host and guest alike |
| `GET` | `/` | Browser client: paste a ticket to join |
| `GET` | `/s/{id}` | Browser terminal; the token comes from the URL fragment |

`host_token` and `guest_token` are shown once and never repeated by another
endpoint or written to a log. Unknown, malformed and expired IDs all answer an
identical 404.

## Configuration

Precedence is **defaults → environment → flags**.

### Relay (`openconsole-server`)

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-listen` | `OPENCONSOLE_LISTEN_ADDR` | `:8080` | Listen address |
| `-session-ttl` | `OPENCONSOLE_SESSION_TTL` | `30m` | Lifetime of an unclaimed session |
| `-log-level` | `OPENCONSOLE_LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` |
| `-healthcheck` | — | — | Probe a running relay and exit |

Durations are Go duration strings (`30m`, `1h30m`). A bare `30` is rejected
rather than guessed at.

### CLI (`openconsole`)

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-server` | `OPENCONSOLE_SERVER` | `http://localhost:8080` | Relay base URL |
| `-shell` | — | `$SHELL` | Shell to run |
| `-version` | — | — | Print version and exit |
| — | `OPENCONSOLE_TICKET` | — | Ticket for `join`, so it stays out of `ps` |

Inside a shared shell, `OPENCONSOLE=1` and `OPENCONSOLE_SESSION` are set, so a
prompt or script can tell it is being watched.

## Development

```sh
gofmt -l .          # must print nothing
go vet ./...
go test ./...
go test -race ./... # the bridge is concurrent; keep this green
```

Build with a version stamp:

```sh
go build -ldflags "-X main.version=v0.2.0" -o bin/ ./cmd/...
```

### Layout

```
cmd/openconsole/          host CLI: share and join
cmd/openconsole-server/   relay server
internal/protocol/        frames, message types, wire encodings — no transport
internal/session/         IDs, tokens, TTL, store, host↔guest bridge — no HTTP
internal/tunnel/          transport; the only package that knows WebSockets
internal/terminal/        PTY and shell handling
internal/server/          HTTP API, tunnel endpoint, config, lifecycle
internal/client/          CLI: share, join, relay API client
internal/webui/           embeds and serves the built browser client
web/                      browser client sources (TypeScript, Vite, xterm.js)
brand/                    logo, icons, social image
deploy/                   compose file and proxy configuration
docs/                     architecture.md, protocol.md
```

### Browser client

```sh
npm --prefix web install
npm --prefix web run build   # bundles into internal/webui/dist
npm --prefix web test
```

`internal/webui/dist` is generated but committed, so a Go-only checkout builds a
working UI. Rebuild and commit it alongside any change under `web/src`. See
[web/README.md](web/README.md).

## Design notes

- **Terminal data is never coupled to WebSockets.** `internal/protocol`
  describes frames; only `internal/tunnel` imports a WebSocket library. The
  session bridge goes further and declares its own `Stream` interface, so
  session management imports no transport at all and its fan-out is tested
  against an in-memory pipe.
- **Terminal bytes stay binary.** `DATA` frames are raw — no JSON, no base64,
  which would cost ~33% on every keystroke. Control messages are JSON and
  self-describing, so a packet capture is readable.
- **Public identifiers are separate from credentials.** `session_id` is safe to
  log and print; the tokens are not, and nothing logs them. Credentials travel
  in the `OPEN` frame rather than a URL, so they stay out of access logs,
  browser history and `Referer` headers. That is why there is one tunnel
  endpoint instead of a path per session.
- **All identifiers come from `crypto/rand`** — 128 bits for session IDs, 256
  for tokens, compared with `subtle.ConstantTimeCompare`. `math/rand` is never
  used.
- **Nobody's terminal freezes for someone else.** A guest that falls behind is
  disconnected rather than allowed to apply backpressure; if the relay stops
  keeping up, the host stops sharing and the local shell carries on.
- **The browser link is a capability URL, by design.** The token sits in the
  fragment (`/s/<id>#<token>`), which browsers never transmit — so the relay
  sees only the session path, and the credential stays out of access logs and
  `Referer` headers while surviving a copy-paste.

## Security

Not yet suitable for exposure to the public internet:

- Session creation is unauthenticated and unrate-limited.
- The relay sees plaintext terminal traffic — it can read and inject keystrokes.
  End-to-end encryption is roadmap, so "self-hosted" is doing real work here.
- Anyone holding a ticket or link has full write access; read-only guests are
  roadmap. A link is a bearer capability and cannot be revoked short of ending
  the session.
- TLS is assumed to be terminated by a proxy in front of the relay.

The full list is in
[docs/architecture.md](docs/architecture.md#known-gaps).

## License

Not yet chosen.
