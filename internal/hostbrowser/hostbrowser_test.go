package hostbrowser

import "testing"

func TestSanitize(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://example.com/auth", "https://example.com/auth", true},
		{"http://localhost:8080/cb", "http://localhost:8080/cb", true},
		{"http://localhost:1455/cb", "http://127.0.0.1:1455/cb", true},
		{"https://x?redirect_uri=http%3A%2F%2Flocalhost%3A1455%2F", "https://x?redirect_uri=http%3A%2F%2F127.0.0.1%3A1455%2F", true},
		{"file:///etc/passwd", "file:///etc/passwd", false},
		{"mailto:a@b.c", "mailto:a@b.c", false},
		{"-injection", "-injection", false},
	}
	for _, tt := range tests {
		got, ok := Sanitize(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("Sanitize(%q) = %q,%v want %q,%v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
