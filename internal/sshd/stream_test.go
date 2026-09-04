package sshd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// fakeChannel is a minimal ssh.Channel.
//
// Write is deliberately left unsynchronised: the Stream contract makes
// serialising writes the transport adapter's job, so running this under -race
// is what proves channelStream actually does it.
type fakeChannel struct {
	written []byte

	readMu  sync.Mutex
	toRead  [][]byte
	readErr error

	stderr   bytes.Buffer
	requests []string
	closed   bool
	mu       sync.Mutex
}

func (f *fakeChannel) Write(p []byte) (int, error) {
	f.written = append(f.written, p...)
	return len(p), nil
}

func (f *fakeChannel) Read(p []byte) (int, error) {
	f.readMu.Lock()
	defer f.readMu.Unlock()
	if len(f.toRead) == 0 {
		if f.readErr != nil {
			return 0, f.readErr
		}
		return 0, io.EOF
	}
	n := copy(p, f.toRead[0])
	f.toRead = f.toRead[1:]
	return n, nil
}

func (f *fakeChannel) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeChannel) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeChannel) CloseWrite() error { return nil }

func (f *fakeChannel) SendRequest(name string, _ bool, _ []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, name)
	return true, nil
}

func (f *fakeChannel) Stderr() io.ReadWriter { return &f.stderr }

var _ ssh.Channel = (*fakeChannel)(nil)

// The bridge fans in from several goroutines, so the adapter has to serialise
// writes itself. Under -race this fails loudly if it does not.
func TestChannelStreamConcurrentSend(t *testing.T) {
	ch := &fakeChannel{}
	s := newChannelStream(ch)
	ctx := context.Background()

	const writers, each = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := s.Send(ctx, protocol.NewData([]byte("xy"))); err != nil {
					t.Errorf("Send: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got, want := len(ch.written), writers*each*2; got != want {
		t.Fatalf("wrote %d bytes, want %d", got, want)
	}
}

func TestChannelStreamMapsFrames(t *testing.T) {
	ctx := context.Background()

	t.Run("DATA becomes bytes", func(t *testing.T) {
		ch := &fakeChannel{}
		s := newChannelStream(ch)
		payload := []byte{0x1b, '[', 'A', 0x00, 0xff}
		if err := s.Send(ctx, protocol.NewData(payload)); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if !bytes.Equal(ch.written, payload) {
			t.Fatalf("wrote %v, want %v", ch.written, payload)
		}
	})

	t.Run("RESIZE and PING are dropped", func(t *testing.T) {
		// An SSH client owns its own window, and SSH has its own keepalives.
		ch := &fakeChannel{}
		s := newChannelStream(ch)
		for _, tp := range []protocol.Type{protocol.TypeResize, protocol.TypePing, protocol.TypePong} {
			f, err := protocol.NewControl(tp, protocol.Resize{Cols: 1, Rows: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Send(ctx, f); err != nil {
				t.Fatalf("Send(%s): %v", tp, err)
			}
		}
		if len(ch.written) != 0 {
			t.Fatalf("control frames reached the terminal: %q", ch.written)
		}
	})

	t.Run("CLOSE passes the exit status through", func(t *testing.T) {
		ch := &fakeChannel{}
		s := newChannelStream(ch)
		code := 7
		f, err := protocol.NewControl(protocol.TypeClose,
			protocol.Close{Reason: "host shell exited", ExitCode: &code})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Send(ctx, f); err != nil {
			t.Fatalf("Send: %v", err)
		}
		// `ssh <session>@relay && echo ok` should behave like a real shell.
		if len(ch.requests) != 1 || ch.requests[0] != "exit-status" {
			t.Fatalf("requests = %v, want one exit-status", ch.requests)
		}
		if !ch.isClosed() {
			t.Fatal("CLOSE did not close the channel")
		}
		if !bytes.Contains(ch.stderr.Bytes(), []byte("host shell exited")) {
			t.Fatalf("stderr = %q, should carry the reason", ch.stderr.String())
		}
	})

	t.Run("ERROR reaches the user", func(t *testing.T) {
		ch := &fakeChannel{}
		s := newChannelStream(ch)
		f, err := protocol.NewControl(protocol.TypeError,
			protocol.Error{Code: "session_not_found", Message: "the host is not connected"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Send(ctx, f); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if !bytes.Contains(ch.stderr.Bytes(), []byte("host is not connected")) {
			t.Fatalf("stderr = %q", ch.stderr.String())
		}
	})
}

func TestChannelStreamRecv(t *testing.T) {
	ch := &fakeChannel{toRead: [][]byte{[]byte("ls -la\r")}}
	s := newChannelStream(ch)

	f, err := s.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if f.Type != protocol.TypeData {
		t.Fatalf("type = %s, want DATA", f.Type)
	}
	if string(f.Payload) != "ls -la\r" {
		t.Fatalf("payload = %q", f.Payload)
	}

	// A closed channel must surface as EOF, not a transport error, so the
	// bridge treats it as an ordinary disconnect.
	if _, err := s.Recv(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after EOF = %v, want io.EOF", err)
	}
}

func TestChannelStreamCloseIsIdempotent(t *testing.T) {
	ch := &fakeChannel{}
	s := newChannelStream(ch)
	if err := s.Close("done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close("again"); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
