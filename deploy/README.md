# Deploying an OpenConsole relay

The relay is a single static binary with no database, no cache and no disk
state. Sessions live in memory and die with the process, which is what makes it
this easy to run.

## Docker

```sh
docker build -t openconsole-server:dev .
docker run --rm -p 8080:8080 openconsole-server:dev
```

Or with compose, from the repository root:

```sh
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml logs -f
```

Point a client at it:

```sh
openconsole -server http://localhost:8080
```

### What the image is

A `scratch` image containing one static binary, running as uid 65532. There is
no shell, no package manager and no libc, so `docker exec` into it will not
work — that is the point. The `HEALTHCHECK` invokes the binary's own
`-healthcheck` flag, which probes `/health` over loopback.

### Configuration

Every setting is an environment variable; see the table in the top-level README.

| Variable | Default |
| --- | --- |
| `OPENCONSOLE_LISTEN_ADDR` | `:8080` |
| `OPENCONSOLE_SESSION_TTL` | `30m` |
| `OPENCONSOLE_LOG_LEVEL` | `info` |
| `OPENCONSOLE_SSH_ADDR` | unset — SSH joins disabled |
| `OPENCONSOLE_SSH_HOST_KEY` | unset — ephemeral key |

Logs are JSON on stderr. With SSH off there is nothing to mount and the
container runs read-only with no volumes at all.

## SSH joins

Guests can join with a stock ssh client and nothing installed:

```sh
ssh <session-id>@console.example.com
```

The session ID is the username; the guest token is the answer to the `Session
token:` prompt. The token is never on the command line, so it stays out of the
guest's shell history and out of `ps`.

**SSH is off unless you turn it on.** An upgrade should not start listening on a
new port unasked, and the host key is a decision only you can make. The compose
file turns it on:

```yaml
environment:
  OPENCONSOLE_SSH_ADDR: ":2222"
  OPENCONSOLE_SSH_HOST_KEY: "/var/lib/openconsole/ssh_host_key"
volumes:
  - openconsole-state:/var/lib/openconsole
```

### The host key must persist

This is the one piece of state the relay writes. SSH clients pin a host key on
first connection, so a key that changes on restart greets every returning guest
with the warning that normally means an active attack — and teaches them to
ignore it.

Point `OPENCONSOLE_SSH_HOST_KEY` at a path on a volume. The relay creates the
key on first start, owner-readable only, in OpenSSH's own format so you can
inspect and rotate it with the tools you already have:

```sh
docker compose -f deploy/docker-compose.yml exec relay true   # no shell in the image
ssh-keygen -l -f /var/lib/docker/volumes/.../ssh_host_key      # or from the host
```

The relay logs the fingerprint at every start:

```
{"msg":"ssh listening","addr":"[::]:2222","host_key":"SHA256:stww…"}
```

Publish that fingerprint so guests can verify the relay on first connection.

Leave `OPENCONSOLE_SSH_HOST_KEY` unset and the relay generates a key per start
and warns loudly. That is fine for a five-minute trial and wrong for anything
anyone connects to twice.

### Port 22

`ssh <session>@host` with no `-p` is the nicer experience. Map it in compose:

```yaml
ports:
  - "22:2222"
```

Only on a host with nothing else on 22 — which usually means a dedicated box,
since you still need your own sshd to administer it. Otherwise keep 2222 and
tell guests to pass `-p 2222`.

### What the SSH server will not do

It brokers a terminal and nothing else. `exec` (`ssh host somecommand`) and
subsystems (SFTP/SCP) are refused, and `direct-tcpip` is refused so the relay
cannot be used as a port-forwarding jump host. The relay never runs a shell for
an SSH connection — the shell it bridges to is the host's, already running on
the host's own machine.

## Behind a reverse proxy

The relay speaks plain HTTP and does not terminate TLS. In any deployment
reachable from outside a trusted network, put a proxy in front of it — session
tokens are bearer credentials and must not cross the network in the clear.

Two requirements the proxy must satisfy:

1. **Forward WebSocket upgrades** on `/api/v1/tunnel`. A proxy that strips the
   `Upgrade` and `Connection` headers will let the REST API work perfectly while
   every terminal silently fails to connect.
2. **Do not impose a short idle or read timeout** on that path. A tunnel
   carrying an idle terminal sends nothing for minutes at a time. The relay's
   own PING every 30 seconds keeps a correctly configured proxy happy, but many
   defaults are shorter than a terminal's idle time.

### Caddy

Caddy handles both automatically, and provisions TLS:

```caddyfile
console.example.com {
	reverse_proxy relay:8080
}
```

### nginx

```nginx
server {
    listen 443 ssl http2;
    server_name console.example.com;

    location / {
        proxy_pass http://relay:8080;
        proxy_http_version 1.1;

        # Required for the tunnel; without these the terminal never connects.
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # An idle terminal is normal. Do not cut it off.
        proxy_read_timeout  3600s;
        proxy_send_timeout  3600s;

        # Terminal output must not be batched, or typing feels laggy.
        proxy_buffering off;
    }
}
```

Then point clients at the proxy, not the relay:

```sh
openconsole -server https://console.example.com
```

SSH is a separate listener and does not go through the HTTP proxy. Expose its
port directly, or put a TCP (layer 4) proxy in front of it — an HTTP reverse
proxy cannot carry it.

## Before exposing this publicly

The relay does not yet authenticate session creation or rate-limit it. Anyone
who can reach `POST /api/v1/sessions` can create sessions and consume memory.
Until that is addressed, run it on a private network, behind an authenticating
proxy, or with the container limits in `docker-compose.yml` doing the bounding.

The full list is in [../docs/architecture.md](../docs/architecture.md).

## Kubernetes, systemd and the rest

Deliberately not provided. The relay is one stateless binary listening on one
port with a health endpoint; it drops into whatever you already run. Adding
manifests here would mean maintaining them.
