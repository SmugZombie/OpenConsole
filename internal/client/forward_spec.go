package client

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ForwardSpec is one -L request: listen here, connect there.
//
// The shape matches ssh's -L, because that is what people already have in their
// fingers:
//
//	-L 8080:localhost:80             listen on 127.0.0.1:8080
//	-L 127.0.0.1:8080:localhost:80   the same, spelled out
//	-L 0.0.0.0:8080:localhost:80     deliberately reachable from elsewhere
type ForwardSpec struct {
	// ListenAddr is where the guest listens, on the guest's own machine.
	ListenAddr string
	// RemoteHost and RemotePort are dialled by the *host*, on the host's
	// machine. That is the point: reaching something only the host can see.
	RemoteHost string
	RemotePort uint16
}

// String renders the spec the way it was asked for.
func (f ForwardSpec) String() string {
	return fmt.Sprintf("%s -> %s", f.ListenAddr,
		net.JoinHostPort(f.RemoteHost, strconv.Itoa(int(f.RemotePort))))
}

// defaultForwardBind is where a two-part spec listens.
//
// Loopback, not every interface: a forward is a hole into someone else's
// network, and opening it to the local network by default would be a surprise.
const defaultForwardBind = "127.0.0.1"

// ParseForwardSpec reads one -L value.
func ParseForwardSpec(spec string) (ForwardSpec, error) {
	parts := strings.Split(strings.TrimSpace(spec), ":")

	var bind, localPort, remoteHost, remotePort string
	switch len(parts) {
	case 3:
		bind, localPort, remoteHost, remotePort = defaultForwardBind, parts[0], parts[1], parts[2]
	case 4:
		bind, localPort, remoteHost, remotePort = parts[0], parts[1], parts[2], parts[3]
	default:
		return ForwardSpec{}, fmt.Errorf(
			"invalid -L %q: expected [bind:]port:host:hostport", spec)
	}

	lp, err := parsePort(localPort)
	if err != nil {
		return ForwardSpec{}, fmt.Errorf("invalid -L %q: local %w", spec, err)
	}
	rp, err := parsePort(remotePort)
	if err != nil {
		return ForwardSpec{}, fmt.Errorf("invalid -L %q: remote %w", spec, err)
	}
	if bind == "" {
		bind = defaultForwardBind
	}
	if remoteHost == "" {
		return ForwardSpec{}, fmt.Errorf("invalid -L %q: no host to connect to", spec)
	}

	return ForwardSpec{
		ListenAddr: net.JoinHostPort(bind, strconv.Itoa(int(lp))),
		RemoteHost: remoteHost,
		RemotePort: rp,
	}, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %q is not between 1 and 65535", s)
	}
	return uint16(n), nil
}

// forwardList collects repeated -L flags.
type forwardList []ForwardSpec

func (f *forwardList) String() string {
	out := make([]string, 0, len(*f))
	for _, spec := range *f {
		out = append(out, spec.String())
	}
	return strings.Join(out, ", ")
}

func (f *forwardList) Set(v string) error {
	spec, err := ParseForwardSpec(v)
	if err != nil {
		return err
	}
	*f = append(*f, spec)
	return nil
}
