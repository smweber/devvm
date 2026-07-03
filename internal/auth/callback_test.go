package auth

import "testing"

func TestCallbackPort(t *testing.T) {
	tests := []struct {
		name string
		url  string
		port int
		ok   bool
	}{
		{"claude random port", "https://claude.ai/oauth/authorize?redirect_uri=http%3A%2F%2Flocalhost%3A54231%2Fcallback&state=x", 54231, true},
		{"127.0.0.1 literal", "https://x/auth?redirect_uri=http://127.0.0.1:8976/cb", 8976, true},
		{"codex 1455 skipped", "https://x/auth?redirect_uri=http%3A%2F%2Flocalhost%3A1455%2F", 0, false},
		{"no redirect_uri", "https://x/auth?client_id=abc", 0, false},
		{"no loopback", "https://x/auth?redirect_uri=https%3A%2F%2Fexample.com%2Fcb", 0, false},
	}
	for _, tt := range tests {
		port, ok := CallbackPort(tt.url)
		if ok != tt.ok || port != tt.port {
			t.Errorf("%s: CallbackPort = %d,%v want %d,%v", tt.name, port, ok, tt.port, tt.ok)
		}
	}
}
