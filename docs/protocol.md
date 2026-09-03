# OpenConsole Protocol

The protocol carries terminal traffic and control messages between a host CLI,
the relay, and a guest.

**Status:** Phase 1 defines the message types and their payload encodings
(`internal/protocol`). No transport implements them yet — that arrives with
`internal/tunnel` in Phase 2. Fixing the shapes now means the transport can be
written against a stable target, and swapped later without touching either end.

## Design decisions

### 1. The protocol does not know about WebSockets

`internal/protocol` imports no transport package. It describes *frames*: a type,
a channel, and a payload. A WebSocket connection is one way to move frames; QUIC,
an SSH channel or a raw TCP socket are others. Only `internal/tunnel` will know
which is in use.

This is the single most important constraint in the design. Terminal streaming
is the part most likely to be re-hosted on a different transport, and coupling it
to a WebSocket would make that a rewrite.

### 2. Terminal data is raw binary; control messages are JSON

Terminal output is an arbitrary byte stream — escape sequences, UTF-8 fragments
split across reads, invalid UTF-8 from a misbehaving program. It is also on the
latency-critical path: every keystroke is a frame.

`DATA` frames therefore carry **raw bytes**, with no JSON wrapper and no base64.
Base64 alone would cost ~33% bandwidth plus an encode/decode on every keystroke,
and JSON string escaping cannot represent arbitrary bytes without it.

Control frames (`OPEN`, `RESIZE`, `PING`, `PONG`, `CLOSE`, `ERROR`) are
**JSON**. They are rare, so the encoding cost is irrelevant, and being
self-describing makes the protocol far easier to extend, version and debug.

A WebSocket transport gets this split for free: binary frames for `DATA`, text
frames for control.

### 3. Channels exist in the header, multiplexing does not exist yet

Every frame has a `ChannelID`. Today every frame uses channel `0`
(`ChannelControl`) and there is exactly one logical stream.

The field is present from the first version because adding TCP forwarding, file
transfer, or a second terminal later will need multiple concurrent streams over
one tunnel, and retrofitting a channel field is a wire-format break. Reserving
four bytes now is cheap; a breaking change later is not.

**Multiplexing is explicitly not implemented.** There is no channel open/close
handshake, no flow control, no per-channel windows. That is Phase 6 work.

### 4. Credentials travel in payloads, never in URLs

The `OPEN` frame carries the session token in its JSON body rather than in a
query string. Query strings end up in server access logs, browser history, and
`Referer` headers. A capability URL is a legitimate pattern, but it has to be
designed as one deliberately — this is not that.

## Message types

| Type | Value | Direction | Payload | Purpose |
| --- | --- | --- | --- | --- |
| `OPEN` | 1 | host→relay, guest→relay | JSON `Open` | Authenticate and attach to a session. |
| `DATA` | 2 | both | **raw bytes** | Terminal input/output. |
| `RESIZE` | 3 | guest→relay→host | JSON `Resize` | Terminal window size changed. |
| `PING` | 4 | both | opaque bytes | Liveness probe. |
| `PONG` | 5 | both | echo of PING | Answer to a probe. |
| `CLOSE` | 6 | both | JSON `Close` | Orderly shutdown. |
| `ERROR` | 7 | both | JSON `Error` | Fatal condition; sender may close after. |

Type values are stable. New types are appended; existing values are never reused.

### Payload schemas

```jsonc
// OPEN
{
  "version": 1,
  "session_id": "…",     // public identifier
  "role": "host",        // "host" | "guest"
  "token": "…",          // HostToken or GuestToken — never logged
  "cols": 120,           // optional initial size
  "rows": 40
}

// RESIZE
{ "cols": 120, "rows": 40 }

// CLOSE
{ "reason": "host exited", "exit_code": 0 }   // both optional

// ERROR
{ "code": "unauthorized", "message": "…" }    // message never contains a credential
```

Error codes: `unauthorized`, `session_not_found`, `session_expired`,
`protocol_error`, `internal_error`, `unsupported_version`.

## Session lifecycle (planned)

```
host                          relay                         guest
 │                              │                             │
 │ POST /api/v1/sessions        │                             │
 │─────────────────────────────▶│                             │
 │◀─── id + host/guest tokens ──│                             │
 │                              │                             │
 │ connect tunnel               │                             │
 │ OPEN{role:host, token}       │                             │
 │─────────────────────────────▶│                             │
 │                              │◀── connect + OPEN{guest} ───│
 │                              │                             │
 │◀──────── DATA (stdin) ───────│◀──────── DATA (stdin) ──────│
 │───────── DATA (stdout) ─────▶│───────── DATA (stdout) ────▶│
 │                              │◀──────── RESIZE ────────────│
 │◀──────── RESIZE ─────────────│                             │
 │                              │                             │
 │ shell exits                  │                             │
 │───────── CLOSE ─────────────▶│───────── CLOSE ────────────▶│
```

Rules that already follow from the model:

- `OPEN` must be the first frame on a connection. Anything else is a
  `protocol_error`.
- A version mismatch is answered with `ERROR{unsupported_version}` and a close.
- An authentication failure is answered with `ERROR{unauthorized}` and a close,
  with no hint about whether the session ID existed.
- When the host disconnects, the session ends and every guest is closed. The
  host is the terminal; there is nothing to attach to without it.

## Framing (Phase 2)

Not yet specified. The transport chooses framing:

- **WebSocket** — the message boundary *is* the frame boundary. Binary messages
  are `DATA`; text messages are control JSON with the type named inside. The
  channel ID becomes a header once multiplexing lands.
- **Raw stream transports** — will need an explicit length-prefixed header
  (`type:uint8`, `channel:uint32`, `length:uint32`, payload), which is exactly
  the `Frame` struct on the wire.

`Frame` is defined so that both encodings describe the same thing.

## Deliberately out of scope for now

- Multiplexed channels and TCP forwarding
- Flow control and backpressure signalling
- Compression
- End-to-end encryption between host and guest (the relay currently sees
  plaintext; transport TLS is assumed)
- Read-only guests and multi-guest write arbitration
- Session recording/replay
