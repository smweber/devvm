package backend

import "testing"

func TestParseConnectTimeout(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		bad  bool
	}{
		{in: "", want: defaultSSHConnectTimeout},
		{in: "30", want: 30},
		{in: "30s", want: 30},
		{in: "1m", want: 60},
		{in: "1500ms", want: 2}, // rounded up: ssh takes whole seconds
		{in: "0", bad: true},
		{in: "-5", bad: true},
		{in: "0s", bad: true},
		{in: "soon", bad: true},
	} {
		got, err := parseConnectTimeout(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: want error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestSSHConnectTimeoutEnv(t *testing.T) {
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "45s")
	if got := SSHConnectTimeout(); got != 45 {
		t.Fatalf("got %d, want 45", got)
	}
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "later")
	if got := SSHConnectTimeout(); got != defaultSSHConnectTimeout {
		t.Fatalf("unparseable value gave %d, want the default %d", got, defaultSSHConnectTimeout)
	}
}
