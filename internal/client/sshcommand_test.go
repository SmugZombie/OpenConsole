package client

import "testing"

// An HTTP proxy in front of the relay rarely carries arbitrary TCP ports, so
// SSH ends up on a name of its own. The relay says where; the client believes
// it rather than guessing from the URL it happens to have been given.
func TestSSHCommand(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		sess    *Session
		want    string
	}{
		{
			name:    "same host as the API",
			baseURL: "https://console.example.com",
			sess:    &Session{SSHPort: 2222},
			want:    "ssh -p 2222 abc@console.example.com",
		},
		{
			name:    "port 22 needs no flag",
			baseURL: "https://console.example.com",
			sess:    &Session{SSHPort: 22},
			want:    "ssh abc@console.example.com",
		},
		{
			// The case this exists for: the site is proxied, ssh is not.
			name:    "ssh on its own name",
			baseURL: "https://openconsole.dev",
			sess:    &Session{SSHPort: 22, SSHHost: "ssh.openconsole.dev"},
			want:    "ssh abc@ssh.openconsole.dev",
		},
		{
			name:    "own name and a non-standard port",
			baseURL: "https://openconsole.dev",
			sess:    &Session{SSHPort: 2222, SSHHost: "ssh.openconsole.dev"},
			want:    "ssh -p 2222 abc@ssh.openconsole.dev",
		},
		{
			name:    "SSH disabled",
			baseURL: "https://console.example.com",
			sess:    &Session{},
			want:    "",
		},
		{
			name:    "no session",
			baseURL: "https://console.example.com",
			sess:    nil,
			want:    "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NewClient(tc.baseURL).SSHCommand("abc", tc.sess)
			if got != tc.want {
				t.Fatalf("SSHCommand = %q, want %q", got, tc.want)
			}
		})
	}
}
