package bootstrap

import (
	"os/exec"
	"slices"
	"testing"
)

// Only language_TERRITORY UTF-8 names go to the guest: they land in its
// sed and grep patterns, and C/POSIX need no generating.
func TestHostLocales(t *testing.T) {
	got := HostLocales([]string{
		"LANG=en_US.UTF-8", "LC_ALL=en_CA.UTF-8", "LC_TIME=en_CA.utf8", "LC_CTYPE=C.UTF-8",
		"LC_MESSAGES=POSIX", "LC_X=en_CA.UTF-8; rm -rf /", "LANGUAGE=fr_CA.UTF-8", "HOME=/de_DE.UTF-8", "LC_PAPER=",
	})
	want := []string{"en_CA.UTF-8", "en_US.UTF-8"}
	if !slices.Equal(got, want) {
		t.Fatalf("HostLocales = %v, want %v", got, want)
	}
}

// Both prereq scripts carry the locale step and still parse.
func TestPrereqScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	for name, script := range map[string]string{"smol": smolPrereqs, "remote": remotePrereqs} {
		if out, err := exec.Command(bash, "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s prereqs do not parse: %v\n%s", name, err, out)
		}
	}
}
