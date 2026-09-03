# OpenConsole browser client

The terminal a guest opens in a browser: TypeScript, Vite and
[xterm.js](https://xtermjs.org).

The build output goes to `../internal/webui/dist` and is embedded into the relay
binary, so the relay stays a single file with no assets to deploy.

## Build

```sh
npm install
npm run build       # typecheck, then bundle into ../internal/webui/dist
```

`internal/webui/dist` is generated but **committed**, so `go build ./...`
produces a relay with a working UI on a machine that has no Node installed.
That is a deliberate trade: the cost is a generated directory in git, and the
rule that comes with it is to rebuild and commit the bundle in the same change
as any edit under `web/src`. The Docker image builds it from source, so a
container is never stale.

## Develop

Run a relay, then the Vite dev server:

```sh
go run ./cmd/openconsole-server      # terminal 1
npm run dev                          # terminal 2, http://localhost:5173
go run ./cmd/openconsole             # terminal 3, share a shell
```

The dev server proxies `/api` and `/health` to the relay on :8080, WebSocket
upgrade included. Open the ticket's session path against the dev server —
`http://localhost:5173/s/<session>#<token>` — to get hot reload against a real
terminal.

## Test

```sh
npm test            # vitest
npm run typecheck
```

The tests cover the wire codec and URL parsing — the two places where a silent
bug corrupts a terminal or drops a credential in the wrong place.

## Layout

```
src/protocol.ts   frame encodings; mirrors Go's internal/protocol exactly
src/tunnel.ts     the WebSocket end of a tunnel; the only transport-aware file
src/locator.ts    session + token parsing out of a URL or a ticket
src/main.ts       views, terminal wiring, font scaling
public/           brand assets, copied verbatim into the bundle
```

## Two things worth knowing

**The token is in the URL fragment.** A guest link is
`/s/<session>#<guest-token>`. Browsers never send a fragment to a server, so the
credential stays out of relay access logs, proxy logs and `Referer` headers,
while still surviving a copy-paste of the whole link. Anything that moves it
into the path or a query string breaks that property.

**The host owns the terminal's size.** Guests are told the size and pick a font
that makes that grid fit their window; they never resize someone else's real
terminal. The relay ignores a `RESIZE` sent by a guest.
