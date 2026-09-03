package server

import "testing"

func TestHealthCheckHost(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{":8080", "127.0.0.1:8080"},
		{"0.0.0.0:8080", "127.0.0.1:8080"},
		{"[::]:8080", "127.0.0.1:8080"},
		{"127.0.0.1:9000", "127.0.0.1:9000"},
		{"localhost:9000", "localhost:9000"},
		{"8080", "127.0.0.1:8080"},
	}
	for _, tc := range tests {
		if got := healthCheckHost(tc.in); got != tc.want {
			t.Errorf("healthCheckHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
