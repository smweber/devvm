package bootstrap

import (
	"os/exec"
	"slices"
	"strings"
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

// The locale step runs right after apt_update and before the package
// install, so the apt and perl calls of a first setup do not warn about
// the host's locale.
func TestPrereqLocalesBeforeInstall(t *testing.T) {
	for name, script := range map[string]string{"smol": smolPrereqs, "remote": remotePrereqs} {
		update := strings.Index(script, "\napt_update\n")
		gen := strings.Index(script, "\ngen_locales \"${@:2}\"")
		install := strings.Index(script, "install -y "+devvmPackages)
		if update < 0 || gen < 0 || install < 0 || !(update < gen && gen < install) {
			t.Errorf("%s prereqs: apt_update at %d, gen_locales at %d, install at %d; want them in that order", name, update, gen, install)
		}
	}
}

// Every locale-gen run is under LC_ALL=C: the session's LC_ALL names the
// locale being made, and locale-gen would warn that it cannot set it. By
// env, so this bash never sets LC_ALL itself (restoring a prefix
// assignment warns the same way).
func TestLocaleGenUnderC(t *testing.T) {
	runs := 0
	for _, l := range strings.Split(genLocales, "\n") {
		if i := strings.Index(l, "locale-gen"); i >= 0 && !strings.Contains(l, "command -v locale-gen") {
			runs++
			if !strings.HasSuffix(l[:i], "env LC_ALL=C ") {
				t.Errorf("locale-gen without env LC_ALL=C: %q", strings.TrimSpace(l))
			}
		}
	}
	if runs != 2 {
		t.Errorf("%d locale-gen runs, want 2", runs)
	}
}
