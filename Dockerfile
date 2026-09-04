# Build the browser client, then the relay as a static binary, then ship only
# that binary.
#
# The result is a scratch image holding one file. There is no shell, package
# manager, or libc, so the usual container attack surface simply is not present
# — which matters for a service whose whole job is brokering other people's
# terminals.

# --- browser client -------------------------------------------------------
FROM node:22-alpine AS web

WORKDIR /web

# Lockfile first: this layer is cached until dependencies change, so editing a
# component does not re-resolve the tree.
COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
# The bundle is committed to the repository so `go build` works without Node,
# but the image rebuilds it from source so a container can never ship a stale
# UI that someone forgot to regenerate.
RUN npm run build

# --- relay ----------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, for the same caching reason as above.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Overwrite the committed bundle with the one just built.
COPY --from=web /internal/webui/dist ./internal/webui/dist

# An empty state directory, owned by the runtime uid. Docker initialises a
# fresh named volume from the image's directory — contents and ownership — so
# this is what lets the unprivileged relay write its SSH host key to a mounted
# volume. A scratch image cannot mkdir at runtime, and a volume created without
# it lands root-owned and unwritable.
RUN mkdir -p /state && chown 65532:65532 /state

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 is what makes the binary runnable on scratch: with cgo, Go's
# net package would link against the host libc for DNS resolution.
# -trimpath keeps build paths out of the binary, so builds are reproducible.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/openconsole-server ./cmd/openconsole-server

# --- runtime --------------------------------------------------------------
FROM scratch

# The relay dials nothing outbound and terminates no TLS itself, but CA roots
# cost 200 KB and mean an operator can add an outbound integration later
# without rebuilding the base.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/openconsole-server /openconsole-server
COPY --from=build --chown=65532:65532 /state /var/lib/openconsole

# Run unprivileged. scratch has no /etc/passwd, so the uid is given numerically;
# 65532 is the conventional "nonroot" uid and needs no account to work.
USER 65532:65532

# 8080 is the HTTP API and the browser client. 2222 is SSH joins, which are
# opt-in: the port is only listened on when OPENCONSOLE_SSH_ADDR is set.
EXPOSE 8080 2222

# SSH is deliberately not enabled here. An image upgrade must never start
# listening on a new port unasked, and SSH needs a host-key decision the
# operator has to make — see deploy/docker-compose.yml for the opt-in.
ENV OPENCONSOLE_LISTEN_ADDR=:8080 \
    OPENCONSOLE_SESSION_TTL=30m \
    OPENCONSOLE_LOG_LEVEL=info

# The binary probes itself, so the image needs no curl or wget.
HEALTHCHECK --interval=30s --timeout=5s --start-period=2s --retries=3 \
    CMD ["/openconsole-server", "-healthcheck"]

# No shell in the image, so this is exec form by necessity as well as by
# preference: the relay runs as PID 1 and receives SIGTERM directly, which is
# what its graceful shutdown is waiting for.
ENTRYPOINT ["/openconsole-server"]
