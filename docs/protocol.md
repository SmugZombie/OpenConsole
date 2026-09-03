# OpenConsole Protocol

The protocol carries terminal traffic and control messages between a host CLI,
the relay, and a guest.

**Status:** implemented. `internal/protocol` defines the frames and their
encodings and `internal/tunnel` carries them over WebSockets; `web/src/protocol.ts`
and `web/src/tunnel.ts` are the browser's mirror of the same two files, and the
pairs must be changed together. Multiplexed channels and end-to-end encryption
remain future work.

## Design decisions

### 1. The protocol does not know about WebSockets

`internal/protocol` imports no transport package. It describes *frames*: a type,
a channel, and a payload. `internal/tunnel` is the only package that knows a
WebSocket exists, and it exposes a single interface:

```go
type Conn interface {
	Send(ctx context.Context, f protocol.Frame) error
	Recv(ctx context.Context) (protocol.Frame, error)
	Close(reason string) error
}
```

Everything above it — the session bridge, the CLI — is written against that.
Adding QUIC or an SSH channel means adding a `Conn` implementation and changing
nothing else. The session bridge goes further and declares its own `Stream`
interface, so session management does not import a transport at all and is
tested against an in-memory pipe.

### 2. Terminal data is raw binary; control messages are JSON

Terminal output is an arbitrary byte stream — escape sequences, UTF-8 fragments
split across reads, invalid UTF-8 from a misbehaving program. It is also on the
latency-critical path: every keystroke is a frame.

`DATA` frames therefore carry **raw bytes**, with no JSON wrapper and no base64.
Base64 alone would cost ~33% bandwidth plus an encode/decode on every keystroke,
and JSON string escaping cannot represent arbitrary bytes without it.

Control frames are **JSON**, and self-describing: the wire carries
`{"type":"RESIZE",...}`, not a magic number. They are rare, so the encoding cost
is irrelevant, and a packet capture stays readable.

### 3. Channels exist in the header, multiplexing does not exist yet

Every frame has a `ChannelID`, and it is on the wire today — the binary header
is `[type:uint8][channel:uint32]`. Every frame currently uses channel `0`.

The field is present from the first version because adding TCP forwarding, file
transfer, or a second terminal later will need multiple concurrent streams over
one tunnel, and retrofitting a channel field is a wire-format break. Five bytes
per frame is a rounding error next to that.

**Multiplexing is explicitly not implemented.** There is no channel open/close
handshake, no flow control, no per-channel windows.

### 4. Credentials travel in payloads, never in URLs

The `OPEN` frame carries the session token in its JSON body. Query strings end
up in server access logs, browser history, and `Referer` headers.

This is why there is one tunnel endpoint, `/api/v1/tunnel`, rather than a path
per session: the URL is identical for every connection and reveals nothing. The
relay's own access log records the path and status, never the credential.

The browser is the one place a token has to travel *in* a URL, because a link is
the only thing you can send someone. It goes in the **fragment**:

```
https://console.example.com/s/<session-id>#<guest-token>
                            └── path ───┘ └── never sent ──┘
```

A fragment is not transmitted in an HTTP request. The relay sees only
`GET /s/<session-id>`, so the credential stays out of its access log, out of any
proxy in between, and out of `Referer` headers — while still surviving a
copy-paste of the whole link. The page reads `location.hash` and puts the token
in an `OPEN` frame like every other client. That is what makes this a designed
capability URL rather than a secret leaked into a URL, and it is the property to
protect if the routing ever changes.

## Message types

| Type | Value | Direction | Payload | Purpose |
| --- | --- | --- | --- | --- |
| `OPEN` | 1 | client→relay, then relay→client | JSON `Open` | Authenticate, then acknowledge. |
| `DATA` | 2 | both | **raw bytes** | Terminal input/output. |
| `RESIZE` | 3 | host→relay→guests | JSON `Resize` | The host's window size changed. |
| `PING` | 4 | both | opaque bytes | Liveness probe. |
| `PONG` | 5 | both | echo of PING | Answer to a probe. |
| `CLOSE` | 6 | both | JSON `Close` | Orderly shutdown. |
| `ERROR` | 7 | both | JSON `Error` | Fatal condition; sender closes after. |

Type values are stable. New types are appended; existing values are never
reused.

### RESIZE only flows outward from the host

The host owns its terminal's size. A guest's `RESIZE` is received and ignored.

With several guests attached, letting any of them resize the host's real
terminal would mean whichever resized last wins, fighting both the other guests
and the host's own window manager. It is also a griefing vector. Guests are told
the size on join and whenever it changes, so a browser client can shape its
renderer to match; negotiating the other direction needs multi-guest
arbitration, which is future work.

### Payload schemas

```jsonc
// OPEN — client to relay
{
  "version": 1,
  "session_id": "…",     // public identifier
  "role": "host",        // "host" | "guest"
  "token": "…",          // HostToken or GuestToken — never logged
  "cols": 120,           // optional initial size
  "rows": 40
}

// OPEN — relay to client, acknowledging attachment. Carries no token.
{ "version": 1, "session_id": "…", "role": "host" }

// RESIZE
{ "cols": 120, "rows": 40 }

// CLOSE
{ "reason": "host shell exited", "exit_code": 0 }   // both optional

// ERROR
{ "code": "unauthorized", "message": "…" }    // message never contains a credential
```

Error codes: `unauthorized`, `session_not_found`, `session_expired`,
`protocol_error`, `internal_error`, `unsupported_version`.

## Framing

Two encodings, selected by frame type.

**Binary (`DATA` only)** — a 5-byte big-endian header, then the payload:

```
 0        1                    5
+--------+--------------------+------------------------+
| type   | channel (uint32)   | payload (raw bytes)    |
+--------+--------------------+------------------------+
```

**Text (every other type)** — a JSON envelope:

```json
{"type":"RESIZE","payload":{"cols":120,"rows":40}}
```

Over WebSockets the message boundary *is* the frame boundary, so no length
prefix is needed: binary messages carry `DATA`, text messages carry control. A
raw byte-stream transport would add a length prefix; that is the transport's
concern, not the protocol's.

The two encodings are not interchangeable, and both directions are enforced. A
`DATA` frame smuggled through the JSON path is rejected, as is a control frame
sent as binary — otherwise a peer would have two ways to say the same thing, and
one of them would bypass the size accounting. Payloads are capped at 1 MiB.

## Session lifecycle

```
host                          relay                         guest
 │                              │                             │
 │ POST /api/v1/sessions        │                             │
 │─────────────────────────────▶│                             │
 │◀─── id + host/guest tokens ──│                             │
 │                              │                             │
 │ GET /api/v1/tunnel (upgrade) │                             │
 │ OPEN{role:host, token}       │                             │
 │─────────────────────────────▶│                             │
 │◀──────── OPEN (ack) ─────────│                             │
 │                              │◀── upgrade + OPEN{guest} ───│
 │                              │───────── OPEN (ack) ───────▶│
 │                              │───────── RESIZE ───────────▶│  size on join
 │                              │───────── DATA ─────────────▶│  scrollback replay
 │                              │                             │
 │───────── DATA (stdout) ─────▶│───────── DATA ─────────────▶│
 │◀──────── DATA (stdin) ───────│◀──────── DATA (stdin) ──────│
 │───────── RESIZE ────────────▶│───────── RESIZE ───────────▶│
 │◀──────── PING ───────────────│───────── PING ─────────────▶│
 │───────── PONG ──────────────▶│◀──────── PONG ──────────────│
 │                              │                             │
 │ shell exits                  │                             │
 │───────── CLOSE ─────────────▶│───────── CLOSE ────────────▶│
```

Rules:

- `OPEN` must be the first frame. Anything else is `protocol_error`.
- The relay's `OPEN` acknowledgement means **attached**, not merely
  authenticated. It is sent only after the connection has joined a live bridge,
  so a client that receives it knows the terminal is there. A second host, or a
  guest arriving before the host, is refused with `ERROR` and never sees an ack.
- An authentication failure is answered with `ERROR{unauthorized}` and a close.
  A wrong token, an unknown session and an expired session are indistinguishable
  in both the code and the timing — tokens are compared with
  `subtle.ConstantTimeCompare`.
- A version mismatch is answered with `ERROR{unsupported_version}`.
- Host and guest tokens are not interchangeable. A guest ticket cannot open a
  host tunnel.
- When the host disconnects, the session ends: every guest is closed and the
  session record is deleted immediately rather than idling out its TTL. The host
  *is* the terminal; there is nothing to attach to without it.

### Joining mid-session

A guest that connects to a terminal already in use would otherwise see a blank
screen until the host next typed. On attach the relay sends:

1. `RESIZE` with the host's current size, so a client can shape its renderer.
2. `DATA` replaying up to 64 KiB of recent output.

### Liveness

The relay sends `PING` every 30 seconds and peers answer `PONG`. This is a
protocol frame rather than a WebSocket ping so that a future transport without
its own ping inherits liveness for free. It also keeps proxies from timing out
an idle terminal, which is otherwise a normal state for one to be in.

### Backpressure

A slow guest must never stall the shared terminal. Each guest has a bounded
outbound queue; one that falls too far behind is disconnected rather than
allowed to apply backpressure to the host. Dropping one viewer is strictly
better than freezing the terminal for everyone.

The host side takes the same position from the other direction: if the relay
stops keeping up, the CLI stops sharing and the local shell carries on. A
person's own terminal never blocks on someone else's connection.

## Deliberately out of scope for now

- Multiplexed channels and TCP forwarding
- Flow control and backpressure signalling between peers
- Compression
- End-to-end encryption between host and guest (the relay currently sees
  plaintext; transport TLS is assumed)
- Read-only guests and multi-guest write arbitration
- Session recording/replay
