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

  with any ssh client:
    ssh -p 2222 x5s5gzxptgfksy3hu75jmcoltm@console.example.com

  watch only (cannot type):
    https://console.example.com/s/x5s5gzxptgfksy3hu75jmcoltm#lckndqddit…

  The ticket grants full control of this terminal. Send it privately,
  and type 'exit' here to end the session.

$ ▏
```

Send the link, and they are in your terminal — no client to install, nothing to
sign up for.

> **Status: Phase 5.** Terminal sharing works end to end — from another
> terminal, a browser, or a stock ssh client. Guests can be read-only, and can
> forward a TCP port when the host allows it. End-to-end encryption is not built
> yet, so the relay sees plaintext. Session creation is unauthenticated, so run
> your relay on a trusted network or behind an authenticating proxy. See
> [the roadmap](docs/architecture.md#roadmap).

## How it works

```
  host machine                    relay server                    guests
 ┌──────────────┐               ┌──────────────┐            ┌──────────────┐
 │ openconsole  │──outbound────▶│  sessions    │◀───────────│   browser    │
 │  shell + PTY │   WebSocket   │  + bridges   │ WebSocket  ├──────────────┤
 └──────────────┘               │  + web UI    │◀───────────│ openconsole  │
                                │  + sshd      │            │     join     │
                                └──────────────┘◀───────────├──────────────┤
                                                    SSH     │ ssh <session>│
                                                            │    @relay    │
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

## Install

```sh
curl -fsSL https://openconsole.dev/install.sh | sh
```

Or with Go: `go install github.com/SmugZombie/OpenConsole/cmd/openconsole@latest`

You only need this to **share** a terminal, or to join from one. To **join**,
a browser needs nothing installed, and neither does
`ssh <session-id>@your-relay`.

Any relay serves its own installer, so a self-hosted one works the same way:
`curl -fsSL https://your-relay/install.sh | sh`.

## Quick start

Install it, then share:

```sh
curl -fsSL https://openconsole.dev/install.sh | sh
openconsole
```

That is the whole thing. It talks to `https://openconsole.dev` by default, so
there is nothing to deploy first. It prints a link and a ticket — send either to
whoever is joining, and they open the link in a browser or run
`openconsole join <ticket>`.

Both ends are now the same shell.

### Running your own relay

The relay is the part you self-host. Point clients at it with `-server`, or
`OPENCONSOLE_SERVER`:

```sh
git clone https://github.com/SmugZombie/OpenConsole.git
cd OpenConsole
go build -o bin/ ./cmd/...

./bin/openconsole-server                       # terminal 1
./bin/openconsole -local                       # terminal 2 — shorthand for
                                               # -server http://localhost:8080
./bin/openconsole join <ticket> -local         # terminal 3
```

### Joining over SSH

Start the relay with SSH enabled and guests need nothing installed at all:

```sh
./bin/openconsole-server -ssh-listen :2222 -ssh-host-key ./ssh_host_key
```

```sh
ssh -p 2222 <session-id>@your-relay
# Session token: <paste the part of the ticket after the dot>
```

The token is answered at a prompt rather than passed as an argument, so it stays
out of shell history and out of `ps`. Keep the host key file: SSH clients pin it,
and regenerating it makes every returning guest see a host-key-changed warning. Type in either. Press **Ctrl-]** to
detach as a guest; type `exit` as the host to end the session for everyone.

Pointing at a relay somewhere else:

```sh
openconsole -server https://console.example.com
openconsole join <ticket> -server https://console.example.com
# or: export OPENCONSOLE_SERVER=https://console.example.com
```

The relay a client talks to sees its plaintext terminal traffic, so run your own
if that matters. `https://openconsole.dev` is a convenience, not a trust
boundary — see [Security](#security).

## Forwarding a port

Let someone reach a service only your machine can see — a database on loopback,
a dev server, something on the office network:

```sh
# you, sharing: nothing is reachable unless you say so
openconsole -allow-forward localhost:5432

# them, joining
openconsole join <ticket> -L 5432:localhost:5432
psql -h 127.0.0.1 -p 5432        # now talking to your database
```

The connection rides the tunnel the terminal is already using. **The relay never
dials anything** — your machine does, and only to the targets you listed.

Forwarding is off unless `-allow-forward` is given. Name targets as `host:port`,
comma-separated. `any` permits everything you can reach; it has to be typed out,
and the banner says what it means. A read-only guest cannot forward at all.

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

`host_token`, `guest_token` and `viewer_token` are shown once and never repeated
by another endpoint or written to a log. The viewer token grants watch-only
access, so it can be handed out without also handing over the keyboard. Unknown, malformed and expired IDs all answer an
identical 404.

## Configuration

Precedence is **defaults → environment → flags**.

### Relay (`openconsole-server`)

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-listen` | `OPENCONSOLE_LISTEN_ADDR` | `:8080` | Listen address |
| `-session-ttl` | `OPENCONSOLE_SESSION_TTL` | `30m` | Lifetime of an unclaimed session |
| `-log-level` | `OPENCONSOLE_LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` |
| `-ssh-listen` | `OPENCONSOLE_SSH_ADDR` | off | Enable SSH joins, e.g. `:2222` |
| `-ssh-host-key` | `OPENCONSOLE_SSH_HOST_KEY` | ephemeral | Host key path, created if absent |
| `-create-rate` | `OPENCONSOLE_CREATE_RATE` | `30` | Session creations per minute per source, `0` disables |
| `-create-burst` | `OPENCONSOLE_CREATE_BURST` | `10` | Creations allowed at once per source |
| `-max-sessions` | `OPENCONSOLE_MAX_SESSIONS` | `512` | Live session ceiling, `0` for none |
| — | `OPENCONSOLE_CREATE_TOKEN` | none | Secret required to create a session |
| `-trusted-proxies` | `OPENCONSOLE_TRUSTED_PROXIES` | none | CIDRs whose `X-Forwarded-For` is believed |
| `-healthcheck` | — | — | Probe a running relay and exit |

**Behind a reverse proxy, set `-trusted-proxies`.** Without it every request
looks like it came from the proxy, so the per-source rate limit becomes one
bucket shared by everyone. `OPENCONSOLE_CREATE_TOKEN` turns an open relay into a
private one; clients present it as `OPENCONSOLE_RELAY_TOKEN`.

SSH is off unless `-ssh-listen` is set: an upgrade should not start listening on
a new port unasked.

Durations are Go duration strings (`30m`, `1h30m`). A bare `30` is rejected
rather than guessed at.

### CLI (`openconsole`)

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-server` | `OPENCONSOLE_SERVER` | `https://openconsole.dev` | Relay base URL |
| `-local` | — | — | Shorthand for `-server http://localhost:8080` |
| `-shell` | — | `$SHELL` | Shell to run |
| `-read-only` | — | — | Join without typing (`join` only) |
| `-allow-forward` | — | none | Targets guests may reach, or `any` (share only) |
| `-L` | — | — | Forward a local port, `[bind:]port:host:hostport` (join only, repeatable) |
| `-version` | — | — | Print version and exit |
| — | `OPENCONSOLE_TICKET` | — | Ticket for `join`, so it stays out of `ps` |
| — | `OPENCONSOLE_RELAY_TOKEN` | — | Secret, if the relay requires one to share |

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
internal/sshd/            SSH listener; joins stock ssh clients to a terminal
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
- **One tunnel, many streams.** Channel 0 is the terminal; every other channel
  is a forwarded TCP connection. The channel field has been in the binary header
  since version 1, so forwarding was added without a wire-format break. Guests
  number their own channels and the relay translates, or one guest's database
  connection would receive another's bytes.
- **Capability lives in the token, not in a flag.** A viewer link is read-only
  because the relay reads the token and says so; no client is trusted to ask for
  less than it could take. The acknowledgement reports what was granted, so a
  guest is told what it may do rather than discovering it by being ignored.
- **All identifiers come from `crypto/rand`** — 128 bits for session IDs, 256
  for tokens, compared with `subtle.ConstantTimeCompare`. `math/rand` is never
  used.
- **Nobody's terminal freezes for someone else.** A guest that falls behind is
  disconnected rather than allowed to apply backpressure; if the relay stops
  keeping up, the host stops sharing and the local shell carries on.
- **One bridge, three kinds of guest.** `internal/session` declares the `Stream`
  interface it needs instead of importing a transport, so a WebSocket, an SSH
  channel and an in-memory pipe all satisfy it. Adding SSH took a ~100-line
  adapter and no change to the fan-out, scrollback, backpressure or teardown
  logic at all.
- **The browser link is a capability URL, by design.** The token sits in the
  fragment (`/s/<id>#<token>`), which browsers never transmit — so the relay
  sees only the session path, and the credential stays out of access logs and
  `Referer` headers while surviving a copy-paste.

## Security

Not yet suitable for exposure to the public internet:

- Session creation is unauthenticated by default. It is rate limited and capped,
  and `OPENCONSOLE_CREATE_TOKEN` closes it entirely, but an open relay is open.
- The relay sees plaintext terminal traffic — it can read and inject keystrokes.
  End-to-end encryption is roadmap, so "self-hosted" is doing real work here.
  Clients default to `https://openconsole.dev`; run your own relay and set
  `OPENCONSOLE_SERVER` if you would rather not trust someone else's.
- A link is a bearer capability and cannot be revoked short of ending the
  session. Hand out the watch-only link when someone only needs to look.
- SSH auth is bounded per connection but not across them; nothing rate-limits
  connections per source.
- TLS is assumed to be terminated by a proxy in front of the relay.

The full list is in
[docs/architecture.md](docs/architecture.md#known-gaps).

## License

[MIT](LICENSE). Copyright (c) 2026 Ron Egli.

The four dependencies that ship inside the binaries are permissively licensed
and compatible with it:

| Dependency | Licence |
| --- | --- |
| `github.com/coder/websocket` | ISC |
| `github.com/creack/pty` | MIT |
| `golang.org/x/crypto`, `x/term`, `x/sys` | BSD-3-Clause |

The browser client bundles [xterm.js](https://xtermjs.org) and its fit and
web-links addons, all MIT.
