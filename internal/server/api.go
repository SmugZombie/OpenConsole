package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	SessionID  string    `json:"session_id"`
	HostToken  string    `json:"host_token"`
	GuestToken string    `json:"guest_token"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	ExpiresIn  int       `json:"expires_in_seconds"`
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

	s, err := a.sessions.Create(r.Context())
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
		SessionID:  s.SessionID,
		HostToken:  s.HostToken,
		GuestToken: s.GuestToken,
		CreatedAt:  s.CreatedAt.UTC(),
		ExpiresAt:  s.ExpiresAt.UTC(),
		ExpiresIn:  int(s.ExpiresAt.Sub(a.now()).Seconds()),
		SSHPort:    a.sshPort,
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
