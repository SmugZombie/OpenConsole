package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// HealthCheck probes a running relay's /health endpoint and reports whether it
// is serving.
//
// This exists so a container image can declare a HEALTHCHECK without shipping
// curl or wget. The image is built from scratch and holds one static binary, so
// the binary has to be able to check itself.
func HealthCheck(ctx context.Context, listenAddr string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := "http://" + healthCheckHost(listenAddr) + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: HTTP %d", resp.StatusCode)
	}

	var h healthResponse
	if err := json.Unmarshal(body, &h); err != nil {
		return fmt.Errorf("health check: unreadable response: %w", err)
	}
	if h.Status != "ok" {
		return fmt.Errorf("health check: status %q", h.Status)
	}
	return nil
}

// healthCheckHost turns a listen address into one that can be dialled.
//
// A listen address is frequently a wildcard (":8080", "0.0.0.0:8080"), which is
// valid to bind but not to connect to, so the host is rewritten to loopback.
func healthCheckHost(listenAddr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		// Not host:port — assume it is a bare port.
		return net.JoinHostPort("127.0.0.1", strings.TrimPrefix(listenAddr, ":"))
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
