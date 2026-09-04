package sshd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/session"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// hostStream is a stand-in for a connected host terminal, so the SSH server can
// be tested against a real bridge without a PTY or a WebSocket.
type hostStream struct {
	in  chan protocol.Frame
	out chan protocol.Frame
	die chan struct{}
}

func newHostStream() *hostStream {
	return &hostStream{
		in:  make(chan protocol.Frame, 64),
		out: make(chan protocol.Frame, 256),
		die: make(chan struct{}),
	}
}

func (h *hostStream) Send(ctx context.Context, f protocol.Frame) error {
	f.Payload = append([]byte(nil), f.Payload...)
	select {
	case h.out <- f:
		return nil
	case <-h.die:
		return io.EOF
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *hostStream) Recv(ctx context.Context) (protocol.Frame, error) {
	select {
	case f := <-h.in:
		return f, nil
	case <-h.die:
		return protocol.Frame{}, io.EOF
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	}
}

func (h *hostStream) Close(string) error {
	select {
	case <-h.die:
	default:
		close(h.die)
	}
	return nil
}

// harness is a running SSH relay with one live session and attached host.
type harness struct {
	t        *testing.T
	addr     string
	sess     *session.Session
	host     *hostStream
	bridges  *session.Bridges
	sessions *session.Manager
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	log := discardLogger()
	sessions := session.NewManager(session.Options{TTL: time.Minute})
	t.Cleanup(sessions.Close)
	bridges := session.NewBridges(log)

	sess, err := sessions.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	hostKey, err := GenerateHostKey()
	if err != nil {
		t.Fatalf("GenerateHostKey: %v", err)
	}

	srv, err := New(Options{
		Addr:     "127.0.0.1:0",
		HostKey:  hostKey,
		Sessions: sessions,
		Bridges:  bridges,
		Log:      log,
		Version:  "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Run(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close() })

	// Attach a host so there is a terminal to join.
	bridge, err := bridges.Open(sess.SessionID)
	if err != nil {
		t.Fatalf("bridges.Open: %v", err)
	}
	host := newHostStream()
	go func() { _ = bridge.ServeHost(ctx, host, 100, 40) }()
	t.Cleanup(func() { host.Close("") })

	deadline := time.After(3 * time.Second)
	for !bridge.HostAttached() {
		select {
		case <-deadline:
			t.Fatal("host never attached")
		case <-time.After(time.Millisecond):
		}
	}

	return &harness{t: t, addr: srv.Addr(), sess: sess, host: host, bridges: bridges, sessions: sessions}
}

// dial connects a real SSH client using password auth.
func (h *harness) dial(user, token string) (*ssh.Client, error) {
	return ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(token)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

// join opens an interactive session the way `ssh host` does.
func (h *harness) join(client *ssh.Client) (*ssh.Session, io.WriteCloser, io.Reader) {
	h.t.Helper()

	s, err := client.NewSession()
	if err != nil {
		h.t.Fatalf("NewSession: %v", err)
	}
	if err := s.RequestPty("xterm", 40, 100, ssh.TerminalModes{}); err != nil {
		h.t.Fatalf("RequestPty: %v", err)
	}
	stdin, err := s.StdinPipe()
	if err != nil {
		h.t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := s.StdoutPipe()
	if err != nil {
		h.t.Fatalf("StdoutPipe: %v", err)
	}
	if err := s.Shell(); err != nil {
		h.t.Fatalf("Shell: %v", err)
	}
	return s, stdin, stdout
}

// hostRecv waits for a frame of the given type from the host's side.
func (h *harness) hostRecv(want protocol.Type) protocol.Frame {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-h.host.out:
			if f.Type == want {
				return f
			}
		case <-deadline:
			h.t.Fatalf("timed out waiting for %s from the host side", want)
		}
	}
}

// readUntil reads from r until want appears.
func readUntil(t *testing.T, r io.Reader, want string, timeout time.Duration) string {
	t.Helper()

	var sb strings.Builder
	found := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if strings.Contains(sb.String(), want) {
					close(found)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	select {
	case <-found:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %q; got %q", want, sb.String())
	}
	return sb.String()
}

// The headline: a stock ssh client joins a shared terminal, both ways.
func TestSSHGuestJoinsTerminal(t *testing.T) {
	h := newHarness(t)

	client, err := h.dial(h.sess.SessionID, h.sess.GuestToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, stdin, stdout := h.join(client)
	defer sess.Close()

	readUntil(t, stdout, "attached", 5*time.Second)

	// Host output reaches the ssh client, bytes intact.
	out := []byte("$ ls\x1b[0m\r\n")
	h.host.in <- protocol.NewData(out)
	readUntil(t, stdout, "$ ls", 5*time.Second)

	// Keystrokes reach the host.
	if _, err := stdin.Write([]byte("whoami\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if f := h.hostRecv(protocol.TypeData); string(f.Payload) != "whoami\r" {
		t.Fatalf("host received %q, want %q", f.Payload, "whoami\r")
	}
}

func TestSSHGuestGetsScrollbackOnJoin(t *testing.T) {
	h := newHarness(t)

	h.host.in <- protocol.NewData([]byte("output before joining\r\n"))
	time.Sleep(100 * time.Millisecond)

	client, err := h.dial(h.sess.SessionID, h.sess.GuestToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	_, _, stdout := h.join(client)
	readUntil(t, stdout, "output before joining", 5*time.Second)
}

// A viewer ticket over SSH watches and cannot type.
func TestSSHViewerIsReadOnly(t *testing.T) {
	h := newHarness(t)

	client, err := h.dial(h.sess.SessionID, h.sess.ViewerToken)
	if err != nil {
		t.Fatalf("dial with the viewer token: %v", err)
	}
	defer client.Close()

	sess, stdin, stdout := h.join(client)
	defer sess.Close()

	// The banner has to say so, or someone types for a while and wonders why
	// the terminal is ignoring them.
	readUntil(t, stdout, "read-only", 5*time.Second)

	// Watching works.
	h.host.in <- protocol.NewData([]byte("host output\r\n"))
	readUntil(t, stdout, "host output", 5*time.Second)

	// Typing does not reach the host.
	if _, err := stdin.Write([]byte("rm -rf /\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case f := <-h.host.out:
		t.Fatalf("a read-only guest reached the host with a %s frame: %q", f.Type, f.Payload)
	case <-time.After(500 * time.Millisecond):
	}
}

// The viewer token must not be usable as a host credential.
func TestSSHViewerTokenIsNotAHostCredential(t *testing.T) {
	h := newHarness(t)

	// SSH only ever authenticates guests, so the viewer token authenticates —
	// but read-only, which the bridge test above covers. What must not happen
	// is the host token behaving like a guest one.
	if client, err := h.dial(h.sess.SessionID, h.sess.HostToken); err == nil {
		client.Close()
		t.Fatal("the host token authenticated an SSH guest")
	}
}

func TestSSHRejectsBadCredentials(t *testing.T) {
	h := newHarness(t)

	other, err := h.sessions.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		user  string
		token string
	}{
		{"wrong token", h.sess.SessionID, other.GuestToken},
		{"host token", h.sess.SessionID, h.sess.HostToken},
		{"empty token", h.sess.SessionID, ""},
		{"unknown session", "aaaaaaaaaaaaaaaaaaaaaaaaaa", h.sess.GuestToken},
		{"malformed session", "../../etc/passwd", h.sess.GuestToken},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, err := h.dial(tc.user, tc.token)
			if err == nil {
				client.Close()
				t.Fatal("authentication should have failed")
			}
			// Every failure must read the same, or the error becomes an oracle
			// for which sessions exist.
			if !strings.Contains(err.Error(), "unable to authenticate") &&
				!strings.Contains(err.Error(), "invalid session or token") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// The host token must not open a guest SSH session: the roles are separate.
func TestSSHHostTokenIsNotAGuestCredential(t *testing.T) {
	h := newHarness(t)
	if client, err := h.dial(h.sess.SessionID, h.sess.HostToken); err == nil {
		client.Close()
		t.Fatal("the host token authenticated an SSH guest")
	}
}

// The relay runs nothing. `ssh host <command>` must be refused rather than
// silently behaving like an interactive join.
func TestSSHRejectsExec(t *testing.T) {
	h := newHarness(t)

	client, err := h.dial(h.sess.SessionID, h.sess.GuestToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	if err := sess.Run("rm -rf /"); err == nil {
		t.Fatal("exec should have been rejected")
	}
}

// No port forwarding through the relay.
func TestSSHRejectsPortForwarding(t *testing.T) {
	h := newHarness(t)

	client, err := h.dial(h.sess.SessionID, h.sess.GuestToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if conn, err := client.Dial("tcp", "127.0.0.1:9"); err == nil {
		conn.Close()
		t.Fatal("direct-tcpip should have been rejected")
	}
}

func TestSSHRejectsSubsystem(t *testing.T) {
	h := newHarness(t)

	client, err := h.dial(h.sess.SessionID, h.sess.GuestToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	if err := sess.RequestSubsystem("sftp"); err == nil {
		t.Fatal("sftp subsystem should have been rejected")
	}
}

// A guest cannot resize the host's real terminal.
func TestSSHGuestCannotResizeHost(t *testing.T) {
	h := newHarness(t)

	client, err := h.dial(h.sess.SessionID, h.sess.GuestToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, _, stdout := h.join(client)
	readUntil(t, stdout, "attached", 5*time.Second)

	if err := sess.WindowChange(10, 20); err != nil {
		t.Fatalf("WindowChange: %v", err)
	}

	// Nothing may reach the host, and its size must be unchanged.
	select {
	case f := <-h.host.out:
		t.Fatalf("guest window change produced a %s frame to the host", f.Type)
	case <-time.After(300 * time.Millisecond):
	}
	bridge, _ := h.bridges.Get(h.sess.SessionID)
	if c, r := bridge.Size(); c != 100 || r != 40 {
		t.Fatalf("host size became %dx%d", c, r)
	}
}

// When the host exits, the ssh client must see the exit status, not a hang.
func TestSSHHostExitEndsSession(t *testing.T) {
	h := newHarness(t)

	client, err := h.dial(h.sess.SessionID, h.sess.GuestToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, _, stdout := h.join(client)
	readUntil(t, stdout, "attached", 5*time.Second)

	code := 7
	f, err := protocol.NewControl(protocol.TypeClose,
		protocol.Close{Reason: "host shell exited", ExitCode: &code})
	if err != nil {
		t.Fatal(err)
	}
	h.host.in <- f

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case err := <-done:
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			// The host shell's status is passed through, so
			// `ssh ... && echo ok` behaves like a real shell.
			if exitErr.ExitStatus() != code {
				t.Fatalf("exit status = %d, want %d", exitErr.ExitStatus(), code)
			}
			return
		}
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ssh session did not end when the host exited")
	}
}

func TestSSHGuestWithoutHostIsRefused(t *testing.T) {
	h := newHarness(t)

	// A session that authenticates but has no live terminal.
	orphan, err := h.sessions.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	client, err := h.dial(orphan.SessionID, orphan.GuestToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	stderr, err := sess.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	// The credential was fine; the message must say so rather than implying
	// a bad token.
	readUntil(t, stderr, "host is not connected", 5*time.Second)
}

func TestSSHKeyboardInteractiveAuth(t *testing.T) {
	h := newHarness(t)

	// This is the method a real `ssh` invocation uses: prompt, echo off.
	client, err := ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User: h.sess.SessionID,
		Auth: []ssh.AuthMethod{
			ssh.KeyboardInteractive(func(_, _ string, questions []string, echos []bool) ([]string, error) {
				if len(questions) != 1 {
					t.Errorf("got %d prompts, want 1", len(questions))
					return nil, errors.New("unexpected prompt count")
				}
				if !strings.Contains(strings.ToLower(questions[0]), "token") {
					t.Errorf("prompt = %q, should mention the token", questions[0])
				}
				if len(echos) == 1 && echos[0] {
					t.Error("the token prompt must not echo")
				}
				return []string{h.sess.GuestToken}, nil
			}),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("keyboard-interactive dial: %v", err)
	}
	client.Close()
}
