package client

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// A forwarded connection reaches whatever the host machine can reach — a
// database on localhost, something on the office network, a cloud metadata
// endpoint. That is a much larger capability than typing into a terminal, so
// forwarding is off unless the host asks for it, and when they do they say what
// may be reached.
//
// The wildcard exists because sometimes you genuinely do mean "anything I can
// reach", but it has to be typed out, and the CLI says so out loud when used.

// AllowAny is the wildcard that permits any target.
const AllowAny = "any"

// Allowlist decides which forward targets the host will dial.
//
// The zero value denies everything, which is the default.
type Allowlist struct {
	any     bool
	targets map[string]struct{}
}

// ParseAllowlist reads a comma-separated list of host:port targets.
//
// An empty string denies everything. The literal "any" permits everything.
func ParseAllowlist(spec string) (Allowlist, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Allowlist{}, nil
	}
	if strings.EqualFold(spec, AllowAny) {
		return Allowlist{any: true}, nil
	}

	list := Allowlist{targets: make(map[string]struct{})}
	for _, raw := range strings.Split(spec, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		host, port, err := net.SplitHostPort(entry)
		if err != nil {
			return Allowlist{}, fmt.Errorf("invalid forward target %q: expected host:port", entry)
		}
		if host == "" {
			return Allowlist{}, fmt.Errorf("invalid forward target %q: no host", entry)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Allowlist{}, fmt.Errorf("invalid forward target %q: bad port", entry)
		}
		list.targets[canonicalTarget(host, uint16(n))] = struct{}{}
	}
	if len(list.targets) == 0 {
		return Allowlist{}, nil
	}
	return list, nil
}

// Enabled reports whether any forwarding is permitted at all.
func (a Allowlist) Enabled() bool { return a.any || len(a.targets) > 0 }

// AllowsAny reports whether the wildcard was used.
func (a Allowlist) AllowsAny() bool { return a.any }

// Allows reports whether a target may be dialled.
//
// Matching is on the literal host as written, not on what it resolves to.
// Resolving first would mean a name that points somewhere harmless at
// configuration time could point elsewhere by the time it is dialled, and it
// would turn every check into a DNS lookup a guest can trigger.
func (a Allowlist) Allows(host string, port uint16) bool {
	if a.any {
		return true
	}
	_, ok := a.targets[canonicalTarget(host, port)]
	return ok
}

// String renders the list for the banner.
func (a Allowlist) String() string {
	switch {
	case a.any:
		return AllowAny
	case len(a.targets) == 0:
		return "none"
	}
	out := make([]string, 0, len(a.targets))
	for t := range a.targets {
		out = append(out, t)
	}
	// Sorted so the banner reads the same every run.
	sortStrings(out)
	return strings.Join(out, ", ")
}

// canonicalTarget normalises a target so "LOCALHOST:5432" and "localhost:5432"
// are the same entry.
func canonicalTarget(host string, port uint16) string {
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(int(port)))
}

// sortStrings is a tiny insertion sort, used on a list that is at most a
// handful of entries; pulling in sort for it would be noise.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
