// Package sshd lets a guest join a shared terminal with a stock ssh client:
//
//	ssh <session-id>@console.example.com
//
// Nothing to install, which is the point — the person you are helping may not
// have OpenConsole and may not want it.
//
// The relay never runs a shell for these connections. It is a broker: an SSH
// channel is adapted to session.Stream and joined to the same bridge a browser
// or a CLI guest would attach to.
package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/session"
)

const (
	// handshakeTimeout bounds an unauthenticated connection. Without it,
	// opening sockets and never completing a handshake pins relay memory.
	handshakeTimeout = 30 * time.Second

	// maxAuthTries limits guesses per connection. The token has 256 bits of
	// entropy so guessing is hopeless anyway; this bounds the work an
	// unauthenticated peer can make the relay do.
	maxAuthTries = 3
)

// authPrompt is what the guest sees. It says "token", not "password", because
// that is what they were sent and what they should paste.
const authPrompt = "Session token: "

// Sessions is the part of session.Manager this package needs.
type Sessions interface {
	Authenticate(ctx context.Context, id string, role protocol.Role, token string) (*session.Session, error)
}

// Bridges resolves the live bridge for a session.
type Bridges interface {
	Get(id string) (*session.Bridge, bool)
}

// Options configures a Server.
type Options struct {
	// Addr is the listen address, e.g. ":2222".
	Addr string
	// HostKey is the relay's SSH identity.
	HostKey ssh.Signer
	// Sessions authenticates guests.
	Sessions Sessions
	// Bridges resolves live terminals.
	Bridges Bridges
	// Log receives structured output. Nil uses the default logger.
	Log *slog.Logger
	// Version is reported in the SSH banner.
	Version string
}

// Server accepts SSH connections and joins them to shared terminals.
type Server struct {
	cfg    *ssh.ServerConfig
	opts   Options
	log    *slog.Logger
	ln     net.Listener
	conns  sync.WaitGroup
	closed chan struct{}
	once   sync.Once
}

// New builds a Server and binds its listener, so a port conflict surfaces
// before the caller believes the service is up.
func New(opts Options) (*Server, error) {
	if opts.HostKey == nil {
		return nil, errors.New("sshd: a host key is required")
	}
	if opts.Sessions == nil || opts.Bridges == nil {
		return nil, errors.New("sshd: sessions and bridges are required")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	s := &Server{opts: opts, log: log, closed: make(chan struct{})}

	version := opts.Version
	if version == "" {
		version = "dev"
	}
	s.cfg = &ssh.ServerConfig{
		MaxAuthTries: maxAuthTries,
		// The version string is advertised before authentication, so it says
		// what this is and nothing about the host it runs on.
		ServerVersion: "SSH-2.0-OpenConsole",
		// Keyboard-interactive is the primary method: it prompts with echo
		// off and, unlike password auth, clients do not offer to remember the
		// answer in a keychain — a session token is meant to be short-lived.
		KeyboardInteractiveCallback: s.authKeyboardInteractive,
		// Password auth is kept for scripted clients and for `sshpass`-style
		// use, where an interactive prompt is not available.
		PasswordCallback: s.authPassword,
		AuthLogCallback:  s.logAuthAttempt,
		BannerCallback: func(ssh.ConnMetadata) string {
			return fmt.Sprintf("OpenConsole %s — joining terminal session\r\n", version)
		},
	}
	s.cfg.AddHostKey(opts.HostKey)

	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("sshd: listen on %s: %w", opts.Addr, err)
	}
	s.ln = ln
	return s, nil
}

// Addr reports the address actually bound, useful when the configured address
// used port 0.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Run accepts connections until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.log.Info("ssh listening",
		slog.String("addr", s.Addr()),
		slog.String("host_key", Fingerprint(s.opts.HostKey)),
	)

	go func() {
		<-ctx.Done()
		s.Close()
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				s.conns.Wait()
				return nil
			default:
			}
			// A transient accept error should not kill the listener.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			s.conns.Wait()
			return fmt.Errorf("sshd: accept: %w", err)
		}

		s.conns.Add(1)
		go func() {
			defer s.conns.Done()
			// One malformed connection must not take the relay down with it.
			// This goroutine is the top-level entry point for an untrusted
			// peer and nothing above it can recover, so a panic here would
			// end every live terminal on the process — the same reasoning as
			// the HTTP handler chain's recovery.
			defer func() {
				if v := recover(); v != nil {
					s.log.Error("panic serving ssh connection",
						slog.String("remote", host(conn.RemoteAddr())),
						slog.Any("panic", v))
					conn.Close()
				}
			}()
			s.handleConn(ctx, conn)
		}()
	}
}

// Close stops accepting. In-flight connections are ended by cancelling the
// context passed to Run.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		close(s.closed)
		err = s.ln.Close()
	})
	return err
}

/* --- authentication ------------------------------------------------------ */

// sessionIDKey is where the authenticated session lands in the connection's
// permissions, so the channel handler does not have to authenticate again.
const sessionIDKey = "openconsole-session-id"

func (s *Server) authKeyboardInteractive(c ssh.ConnMetadata, client ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
	answers, err := client("", "", []string{authPrompt}, []bool{false}) // echo off
	if err != nil {
		return nil, err
	}
	if len(answers) != 1 {
		return nil, errUnauthorized
	}
	return s.authenticate(c, answers[0])
}

func (s *Server) authPassword(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
	return s.authenticate(c, string(password))
}

// errUnauthorized is what every failure returns.
//
// The message reaches the client, so it must not distinguish an unknown session
// from a wrong token: either would let someone probe which sessions exist.
var errUnauthorized = errors.New("invalid session or token")

func (s *Server) authenticate(c ssh.ConnMetadata, token string) (*ssh.Permissions, error) {
	// The SSH username is the public session ID. It is safe there: unlike the
	// token it is not a credential, and it is what makes `ssh <session>@relay`
	// read naturally.
	id := c.User()

	sess, err := s.opts.Sessions.Authenticate(
		context.Background(), id, protocol.RoleGuest, token)
	if err != nil {
		return nil, errUnauthorized
	}
	return &ssh.Permissions{
		Extensions: map[string]string{sessionIDKey: sess.SessionID},
	}, nil
}

// logAuthAttempt records outcomes without ever recording what was tried.
func (s *Server) logAuthAttempt(c ssh.ConnMetadata, method string, err error) {
	if err == nil {
		s.log.Info("ssh authenticated",
			slog.String("session_id", c.User()),
			slog.String("method", method),
			slog.String("remote", host(c.RemoteAddr())))
		return
	}
	// "none" is the probe every client sends first to discover which methods
	// are offered; logging it as a failure would be noise.
	if method == "none" {
		return
	}
	s.log.Info("ssh authentication failed",
		slog.String("session_id", c.User()),
		slog.String("method", method),
		slog.String("remote", host(c.RemoteAddr())))
}

/* --- connections --------------------------------------------------------- */

func (s *Server) handleConn(ctx context.Context, nConn net.Conn) {
	defer nConn.Close()

	// The deadline covers only the handshake; an authenticated session runs as
	// long as the terminal does.
	_ = nConn.SetDeadline(time.Now().Add(handshakeTimeout))

	conn, chans, reqs, err := ssh.NewServerConn(nConn, s.cfg)
	if err != nil {
		// Failed handshakes are routine: port scanners, health checks, wrong
		// tokens. Debug, not info.
		s.log.Debug("ssh handshake failed",
			slog.String("remote", host(nConn.RemoteAddr())),
			slog.Any("error", err))
		return
	}
	defer conn.Close()
	_ = nConn.SetDeadline(time.Time{})

	// Every authentication path sets this. A connection without it got here
	// some way this code does not know about, so refuse rather than guess.
	sessionID := ""
	if conn.Permissions != nil {
		sessionID = conn.Permissions.Extensions[sessionIDKey]
	}
	if sessionID == "" {
		s.log.Error("authenticated ssh connection carries no session",
			slog.String("remote", host(nConn.RemoteAddr())))
		return
	}

	// Global requests are all things this relay does not do: port forwarding,
	// keepalive extensions, and so on. Rejecting them is the policy.
	go ssh.DiscardRequests(reqs)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	var wg sync.WaitGroup
	for newCh := range chans {
		// "session" is the only channel type a terminal needs. Refusing the
		// rest is what keeps this from becoming a general-purpose SSH server:
		// no direct-tcpip, so no port forwarding through the relay.
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			s.log.Debug("ssh channel accept failed", slog.Any("error", err))
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleSession(ctx, sessionID, ch, chReqs)
		}()
	}
	wg.Wait()
}

// handleSession serves one SSH session channel.
func (s *Server) handleSession(ctx context.Context, sessionID string, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	// Wait for the client to ask for a shell before attaching. Attaching
	// earlier would start streaming terminal output at a client that has not
	// said it wants an interactive session.
	intent := &channelIntent{
		shell: make(chan struct{}),
		abort: make(chan struct{}),
	}
	go s.serveChannelRequests(reqs, ch, intent)

	select {
	case <-intent.shell:
	case <-intent.abort:
		// The client asked for something this relay does not do. It has been
		// told; ending the channel now means it fails immediately rather than
		// hanging until the no-shell timeout.
		return
	case <-ctx.Done():
		return
	case <-time.After(handshakeTimeout):
		fmt.Fprint(ch.Stderr(), "openconsole: no shell was requested\r\n")
		return
	}

	bridge, ok := s.opts.Bridges.Get(sessionID)
	if !ok {
		// The session authenticated, so it existed a moment ago; the host has
		// since gone. Say so plainly rather than implying a bad credential.
		fmt.Fprint(ch.Stderr(), "openconsole: the host is not connected\r\n")
		return
	}

	fmt.Fprint(ch, "openconsole: attached. The host ends the session.\r\n")

	stream := newChannelStream(ch)
	err := bridge.ServeGuest(ctx, stream)
	switch {
	case err == nil,
		errors.Is(err, io.EOF),
		errors.Is(err, context.Canceled),
		errors.Is(err, session.ErrBridgeClosed):
	case errors.Is(err, session.ErrNoHost):
		fmt.Fprint(ch.Stderr(), "openconsole: the host is not connected\r\n")
	default:
		s.log.Info("ssh guest ended",
			slog.String("session_id", sessionID),
			slog.Any("error", err))
	}
}

// channelIntent reports what the client asked the channel to become.
type channelIntent struct {
	// shell closes once an interactive session was requested.
	shell chan struct{}
	// abort closes when the client asked for something unsupported.
	abort chan struct{}
}

// serveChannelRequests answers the client's channel requests, signalling what
// it asked for.
func (s *Server) serveChannelRequests(reqs <-chan *ssh.Request, ch ssh.Channel, intent *channelIntent) {
	var shellOnce, abortOnce sync.Once
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			// Accepted so the client sets up its terminal, but the requested
			// size is discarded: the host owns the terminal's dimensions and
			// a guest does not get to resize someone else's window.
			reply(req, true)

		case "shell":
			reply(req, true)
			shellOnce.Do(func() { close(intent.shell) })

		case "window-change":
			// Same reasoning as pty-req. Acknowledged, ignored.
			reply(req, true)

		case "env":
			// Accepted and dropped. The shell is the host's, already running,
			// with the host's environment; a guest cannot alter it.
			reply(req, true)

		case "exec", "subsystem":
			// The relay executes nothing and serves no SFTP. Allowing these
			// would imply a capability that does not exist.
			fmt.Fprint(ch.Stderr(), "openconsole: only interactive sessions are supported\r\n")
			reply(req, false)
			sendExitStatus(ch, 1)
			abortOnce.Do(func() { close(intent.abort) })

		default:
			reply(req, false)
		}
	}
}

func reply(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

// host strips the port from an address, keeping log lines stable per client.
func host(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	s := addr.String()
	if i := strings.LastIndex(s, ":"); i > 0 {
		return s[:i]
	}
	return s
}
