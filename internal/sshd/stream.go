package sshd

import (
	"context"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// channelStream adapts an SSH session channel to session.Stream.
//
// This is the whole reason the bridge declares its own Stream interface instead
// of importing a transport: an SSH channel is not a WebSocket and speaks no
// framing at all, yet the fan-out, scrollback replay, backpressure and teardown
// logic are reused unchanged. The adapter is the only new code a transport
// needs.
//
// The mapping is asymmetric because SSH already provides what the control
// frames exist to carry:
//
//	inbound  bytes  -> DATA frames
//	outbound DATA   -> bytes
//	outbound RESIZE -> dropped; an SSH client owns its own window
//	outbound PING   -> dropped; SSH has keepalives of its own
//	outbound CLOSE  -> exit-status, then close
//	outbound ERROR  -> written to stderr, then close
type channelStream struct {
	ch ssh.Channel

	// readBuf sizes one inbound DATA frame. Keystrokes are tiny; a paste is
	// the only thing that fills this.
	readBuf []byte

	// writeMu serialises writes, which the Stream contract requires: the
	// bridge's writer goroutine drains terminal output while the reader
	// goroutine answers PING on the same channel.
	writeMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

func newChannelStream(ch ssh.Channel) *channelStream {
	return &channelStream{ch: ch, readBuf: make([]byte, 32<<10)}
}

// Send delivers a frame to the SSH client.
func (s *channelStream) Send(ctx context.Context, f protocol.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	switch f.Type {
	case protocol.TypeData:
		s.writeMu.Lock()
		_, err := s.ch.Write(f.Payload)
		s.writeMu.Unlock()
		if err != nil {
			return fmt.Errorf("sshd: write: %w", err)
		}
		return nil

	case protocol.TypeResize, protocol.TypePing, protocol.TypePong:
		// Nothing to do. An SSH client sizes its own terminal, and SSH runs
		// its own keepalives beneath us.
		return nil

	case protocol.TypeClose:
		var c protocol.Close
		_ = protocol.DecodeControl(f, &c)
		if c.Reason != "" {
			s.writeMu.Lock()
			fmt.Fprintf(s.ch.Stderr(), "\r\nopenconsole: %s\r\n", c.Reason)
			s.writeMu.Unlock()
		}
		// Pass the host shell's exit status through, so `ssh ... && echo ok`
		// behaves the way it would against a real shell.
		code := 0
		if c.ExitCode != nil {
			code = *c.ExitCode
		}
		sendExitStatus(s.ch, code)
		return s.Close(c.Reason)

	case protocol.TypeError:
		var e protocol.Error
		_ = protocol.DecodeControl(f, &e)
		msg := e.Message
		if msg == "" {
			msg = e.Code
		}
		s.writeMu.Lock()
		fmt.Fprintf(s.ch.Stderr(), "\r\nopenconsole: %s\r\n", msg)
		s.writeMu.Unlock()
		sendExitStatus(s.ch, 1)
		return s.Close(msg)

	default:
		return nil
	}
}

// Recv turns inbound bytes into DATA frames.
func (s *channelStream) Recv(ctx context.Context) (protocol.Frame, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Frame{}, err
	}

	n, err := s.ch.Read(s.readBuf)
	if n > 0 {
		// The bridge copies before queueing, so reusing the buffer is safe.
		return protocol.NewData(s.readBuf[:n]), nil
	}
	if err != nil {
		if err == io.EOF {
			return protocol.Frame{}, io.EOF
		}
		return protocol.Frame{}, fmt.Errorf("sshd: read: %w", err)
	}
	// A zero-length read with no error carries no information; ask again.
	return protocol.Frame{Type: protocol.TypePong}, nil
}

// Close shuts the channel down. It is idempotent.
func (s *channelStream) Close(string) error {
	s.closeOnce.Do(func() {
		s.closeErr = s.ch.Close()
	})
	return s.closeErr
}

// sendExitStatus reports an exit code to the client. Failure is ignored: the
// channel is closing either way, and a client that has already gone is not an
// error worth surfacing.
func sendExitStatus(ch ssh.Channel, code int) {
	payload := struct{ Status uint32 }{uint32(code)}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&payload))
}
