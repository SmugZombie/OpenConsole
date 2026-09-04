package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxAPIResponse bounds what the CLI will read from a relay. A hostile or
// broken relay should not be able to exhaust the client's memory.
const maxAPIResponse = 64 << 10

// Session is what the relay returns when a session is created.
type Session struct {
	SessionID   string    `json:"session_id"`
	HostToken   string    `json:"host_token"`
	GuestToken  string    `json:"guest_token"`
	ViewerToken string    `json:"viewer_token"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	ExpiresIn   int       `json:"expires_in_seconds"`
	// SSHPort is set when the relay accepts SSH joins.
	SSHPort int `json:"ssh_port,omitempty"`
}

// apiError is the relay's error body.
type apiError struct {
	Code    string `json:"error"`
	Message string `json:"message"`
}

// Client talks to a relay's REST API.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a client for the relay at baseURL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http: &http.Client{
			// Generous enough for a slow link, short enough that a wedged
			// relay does not hang the CLI indefinitely.
			Timeout: 15 * time.Second,
		},
	}
}

// BaseURL reports the relay this client targets.
func (c *Client) BaseURL() string { return c.baseURL }

// CreateSession asks the relay for a new session.
func (c *Client) CreateSession(ctx context.Context) (*Session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contacting relay at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse))
	if err != nil {
		return nil, fmt.Errorf("reading relay response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return nil, relayError(resp.StatusCode, body)
	}

	var s Session
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("decoding relay response: %w", err)
	}
	if s.SessionID == "" || s.HostToken == "" {
		return nil, fmt.Errorf("relay returned an incomplete session")
	}
	return &s, nil
}

// relayError turns a non-success response into a readable error.
func relayError(status int, body []byte) error {
	var e apiError
	if err := json.Unmarshal(body, &e); err == nil && e.Code != "" {
		if e.Message != "" {
			return fmt.Errorf("relay returned %d %s: %s", status, e.Code, e.Message)
		}
		return fmt.Errorf("relay returned %d %s", status, e.Code)
	}
	return fmt.Errorf("relay returned HTTP %d", status)
}

// TunnelURL is the WebSocket endpoint for this relay.
func (c *Client) TunnelURL() string { return c.baseURL + "/api/v1/tunnel" }

// SSHCommand is the command a guest runs to join with a stock ssh client, or
// "" when the relay does not accept SSH joins.
//
// The host is taken from the relay URL this client was pointed at. An operator
// fronting SSH on a different name or port has to say so themselves — the relay
// cannot know how it is reached from outside.
func (c *Client) SSHCommand(sessionID string, sshPort int) string {
	if sshPort == 0 {
		return ""
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	if sshPort == 22 {
		return fmt.Sprintf("ssh %s@%s", sessionID, u.Hostname())
	}
	return fmt.Sprintf("ssh -p %d %s@%s", sshPort, sessionID, u.Hostname())
}

// JoinURL is the page a guest opens in a browser.
//
// The token goes in the URL *fragment*. Browsers never send a fragment to a
// server, so the credential stays out of the relay's access log, out of any
// proxy in between, and out of Referer headers — while still surviving a
// copy-paste of the whole link. That is what makes this a deliberate
// capability URL rather than a secret leaked into a URL.
func (c *Client) JoinURL(sessionID, guestToken string) string {
	return c.baseURL + "/s/" + url.PathEscape(sessionID) + "#" + url.PathEscape(guestToken)
}
