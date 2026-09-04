package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/session"
	"github.com/SmugZombie/OpenConsole/internal/webui"
)

// maxRequestBody caps request bodies. The API takes no meaningful input yet,
// so anything larger is either a mistake or an attack.
const maxRequestBody = 4 << 10 // 4 KiB

// healthResponse is the body of GET /health.
type healthResponse struct {
	Status   string `json:"status"`
	Version  string `json:"version"`
	Sessions int    `json:"sessions"`
	// Tunnels counts sessions with a host terminal currently connected.
	Tunnels int `json:"tunnels"`
	// SSHPort is the relay's SSH port, or 0 when SSH joins are disabled.
	SSHPort int `json:"ssh_port,omitempty"`
}

// createSessionResponse is returned once, to the creator, and is the only
// place tokens ever leave the process. Subsequent lookups never include them.
type createSessionResponse struct {
	SessionID  string `json:"session_id"`
	HostToken  string `json:"host_token"`
	GuestToken string `json:"guest_token"`
	// ViewerToken grants read-only access. Separate from GuestToken so a
	// watch-only link can be handed out without also handing over the keyboard.
	ViewerToken string    `json:"viewer_token"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	ExpiresIn   int       `json:"expires_in_seconds"`
	// SSHPort is the relay's SSH port, present only when SSH joins are
	// enabled. It is relay capability rather than session state, but it is
	// returned here so the host CLI can print the ssh command without a second
	// round trip at exactly the moment it needs to.
	SSHPort int `json:"ssh_port,omitempty"`
}

// sessionResponse is the public view of a session: no credentials.
type sessionResponse struct {
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	ExpiresIn int       `json:"expires_in_seconds"`
}

// errorResponse is the uniform error body.
type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// API wires the session manager and live bridges to HTTP handlers.
type API struct {
	sessions *session.Manager
	bridges  *session.Bridges
	log      *slog.Logger
	version  string
	now      func() time.Time

	// sshPort is 0 when SSH joins are disabled.
	sshPort int

	// createLimit throttles session creation per source; nil disables it.
	createLimit *rateLimiter
	// createToken, when set, is required to create a session.
	createToken string
	// proxies decides whose X-Forwarded-For is believed when identifying a
	// source. Without it a relay behind a proxy would see every request as
	// coming from the proxy, making the rate limit one shared bucket.
	proxies TrustedProxies

	// baseCtx bounds tunnel connections. A tunnel outlives the HTTP handler
	// that created it, so it cannot use the request context; it needs one tied
	// to the server's lifetime so shutdown can end every tunnel.
	baseCtx context.Context
}

// NewAPI returns an API. A nil logger is replaced with the default logger, and
// a nil context with context.Background.
func NewAPI(sessions *session.Manager, bridges *session.Bridges, log *slog.Logger, version string, baseCtx context.Context) *API {
	if log == nil {
		log = slog.Default()
	}
	if bridges == nil {
		bridges = session.NewBridges(log)
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return &API{
		sessions: sessions,
		bridges:  bridges,
		log:      log,
		version:  version,
		now:      time.Now,
		baseCtx:  baseCtx,
	}
}

// SetSSHPort records the SSH port to advertise to clients. Zero disables the
// advertisement.
func (a *API) SetSSHPort(port int) { a.sshPort = port }

// SetCreatePolicy configures who may create sessions and how often.
func (a *API) SetCreatePolicy(cfg Config) {
	a.createLimit = newRateLimiter(cfg.CreateRatePerMin, cfg.CreateBurst, nil)
	a.createToken = cfg.CreateToken
	a.proxies = cfg.TrustedProxies
}

// Routes builds the HTTP handler for the relay API.
//
// The Go 1.22 pattern router is used rather than a third-party mux: it covers
// method matching and path wildcards, which is all this service needs, and it
// keeps the dependency list empty.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("POST /api/v1/sessions", a.handleCreateSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}", a.handleGetSession)
	// Both roles share one tunnel endpoint; the OPEN frame says which is
	// which. Putting the session in the path would leak it into access logs
	// for no benefit.
	mux.HandleFunc("GET /api/v1/tunnel", a.handleTunnel)

	// The browser client registers its own explicit routes; see webui.Register
	// for why it is not mounted as a catch-all.
	webui.Register(mux)

	return a.withRecovery(a.withLogging(mux))
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:   "ok",
		Version:  a.version,
		Sessions: a.sessions.Len(),
		Tunnels:  a.bridges.Len(),
		SSHPort:  a.sshPort,
	})
}

func (a *API) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	// Bound the body even though it is ignored today, so a future field
	// cannot silently become an unbounded read.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	defer r.Body.Close()

	source := a.proxies.ClientIP(r)

	// The shared secret, when one is configured, is checked before the rate
	// limit: an authorised caller should not be throttled alongside whoever
	// else is probing the relay.
	if a.createToken != "" && !a.authorizedToCreate(r) {
		a.log.InfoContext(r.Context(), "session creation refused",
			slog.String("reason", "bad or missing token"),
			slog.String("remote", source))
		// 401 with a challenge, so a client knows what is missing rather than
		// assuming the relay is broken.
		w.Header().Set("WWW-Authenticate", `Bearer realm="openconsole"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "a token is required to create a session")
		return
	}

	if ok, retry := a.createLimit.allow(source); !ok {
		a.log.InfoContext(r.Context(), "session creation rate limited",
			slog.String("remote", source),
			slog.Duration("retry_after", retry))
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds()+0.999)))
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"too many sessions created from here; try again shortly")
		return
	}

	s, err := a.sessions.Create(r.Context())
	if errors.Is(err, session.ErrTooManySessions) {
		a.log.WarnContext(r.Context(), "session creation refused",
			slog.String("reason", "relay at capacity"),
			slog.Int("sessions", a.sessions.Len()))
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusServiceUnavailable, "at_capacity",
			"this relay is holding as many sessions as it allows")
		return
	}
	if err != nil {
		a.log.ErrorContext(r.Context(), "create session failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "could not create session")
		return
	}

	// Only the session ID is logged. Tokens are credentials and must never
	// reach the log pipeline.
	a.log.InfoContext(r.Context(), "session created",
		slog.String("session_id", s.SessionID),
		slog.Time("expires_at", s.ExpiresAt),
	)

	writeJSON(w, http.StatusCreated, createSessionResponse{
		SessionID:   s.SessionID,
		HostToken:   s.HostToken,
		GuestToken:  s.GuestToken,
		ViewerToken: s.ViewerToken,
		CreatedAt:   s.CreatedAt.UTC(),
		ExpiresAt:   s.ExpiresAt.UTC(),
		ExpiresIn:   int(s.ExpiresAt.Sub(a.now()).Seconds()),
		SSHPort:     a.sshPort,
	})
}

func (a *API) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s, err := a.sessions.Get(r.Context(), id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, sessionResponse{
			SessionID: s.SessionID,
			CreatedAt: s.CreatedAt.UTC(),
			ExpiresAt: s.ExpiresAt.UTC(),
			ExpiresIn: int(s.ExpiresAt.Sub(a.now()).Seconds()),
		})
	case errors.Is(err, session.ErrInvalidID), errors.Is(err, session.ErrNotFound):
		// Malformed, unknown and expired IDs all answer 404 with the same
		// body. Distinguishing them would let a caller probe which IDs once
		// existed.
		writeError(w, http.StatusNotFound, "session_not_found", "no such session")
	default:
		a.log.ErrorContext(r.Context(), "session lookup failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error", "could not look up session")
	}
}

// SweepRateLimiter drops idle rate-limit buckets. The bucket table is keyed by
// a source address, which is attacker-chosen, so leaving it to grow would be
// the very leak this limiter exists to prevent.
func (a *API) SweepRateLimiter() int { return a.createLimit.sweep() }

// authorizedToCreate checks the shared secret.
//
// Compared in constant time: a byte-by-byte comparison would leak the correct
// prefix through timing, the same reasoning as session tokens.
func (a *API) authorizedToCreate(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	presented, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare(
		[]byte(strings.TrimSpace(presented)), []byte(a.createToken)) == 1
}

// withLogging records one structured line per request. Neither query strings
// nor headers are logged, since that is where credentials would appear.
func (a *API) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.log.InfoContext(r.Context(), "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)),
			slog.String("remote", remoteHost(r.RemoteAddr)),
		)
	})
}

// withRecovery keeps one bad request from taking down the relay, which would
// otherwise drop every live session on the process.
func (a *API) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				a.log.ErrorContext(r.Context(), "panic serving request",
					slog.String("path", r.URL.Path),
					slog.Any("panic", v),
				)
				writeError(w, http.StatusInternalServerError, "internal_error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.wroteHeader = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Hijack passes through to the underlying ResponseWriter.
//
// Without this the wrapper would hide http.Hijacker, and every WebSocket
// upgrade would fail with 501 while the REST API kept working perfectly — a
// failure that only shows up when a terminal tries to connect.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("server: %T does not support hijacking", s.ResponseWriter)
	}
	s.status = http.StatusSwitchingProtocols
	return h.Hijack()
}

// Flush passes through so streaming responses are not buffered by the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// remoteHost strips the port from a RemoteAddr, keeping logs stable per client.
func remoteHost(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Session state is per-request and must never be cached by an
	// intermediary, least of all a response that carries tokens.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: code, Message: msg})
}
