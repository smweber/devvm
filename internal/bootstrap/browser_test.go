package bootstrap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// sourceProfile sources the profile.d script in sh (with set -u, as a
// strict login shell might) and reports BROWSER afterwards ("<unset>" when
// it is not set) and the status the source left.
func sourceProfile(t *testing.T, script string, env ...string) (browser string, ok bool) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "devvm.sh")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `set -u; . "$1"; st=$?; printf '%s|%s' "${BROWSER-<unset>}" "$st"`, "_", f)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sourcing the profile failed: %v", err)
	}
	b, st, _ := strings.Cut(string(out), "|")
	return b, st == "0"
}

func TestProfileScriptGuard(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "devvm-open-url")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	notExec := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, shim string
		env        []string
		want       string
	}{
		{"shim present, BROWSER unset", shim, nil, shim},
		{"shim present, BROWSER empty", shim, []string{"BROWSER="}, shim},
		{"shim missing exports nothing", filepath.Join(dir, "gone"), nil, "<unset>"},
		{"shim not executable exports nothing", notExec, nil, "<unset>"},
		{"user's BROWSER wins", shim, []string{"BROWSER=firefox"}, "firefox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sourceProfile(t, profileScript(tc.shim), tc.env...)
			if got != tc.want {
				t.Errorf("BROWSER = %q, want %q", got, tc.want)
			}
			if !ok {
				t.Error("sourcing the profile left a false status")
			}
		})
	}
	want := "# Written by devvm setup: BROWSER opens URLs on the devvm host.\n" +
		"if [ -z \"${BROWSER:-}\" ] && [ -x '/usr/local/bin/devvm-open-url' ]; then\n" +
		"    export BROWSER='/usr/local/bin/devvm-open-url'\n" +
		"fi\n"
	if got := profileScript(backend.ManagedShimPath); got != want {
		t.Errorf("profile.d content:\n%s\nwant:\n%s", got, want)
	}
}

// The write script lands exactly profileScript, staged then renamed, and a
// shim path with a quote in it cannot break out of the file.
func TestWriteProfileScriptContent(t *testing.T) {
	for _, shim := range []string{backend.ManagedShimPath, `/opt/it's here/%s/devvm-open-url`} {
		dir := t.TempDir()
		script := strings.ReplaceAll(writeProfileScript(shim), "/etc/profile.d", dir)
		if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%q: write script failed: %v\n%s", shim, err, out)
		}
		got, err := os.ReadFile(filepath.Join(dir, "devvm.sh"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != profileScript(shim) {
			t.Errorf("%q: wrote\n%s\nwant\n%s", shim, got, profileScript(shim))
		}
		if _, err := os.Stat(filepath.Join(dir, "devvm.sh.new")); !os.IsNotExist(err) {
			t.Errorf("%q: staging file left behind", shim)
		}
		if st, _ := os.Stat(filepath.Join(dir, "devvm.sh")); st.Mode().Perm() != 0o644 {
			t.Errorf("%q: mode %v, want 0644", shim, st.Mode().Perm())
		}
	}
}

// recBackend records every Run with the user it ran as.
type recBackend struct {
	fakeBackend
	users []string
}

func (r *recBackend) Run(ctx context.Context, o backend.ExecOpts, argv ...string) error {
	r.users = append(r.users, o.User)
	return r.fakeBackend.Run(ctx, o, argv...)
}

// Browser installs agent and shim and writes profile.d as root on a
// managed box, smol or remote (both daemons run the agent exec), and does
// nothing at all (no install, no exec) on an unmanaged one of either kind.
func TestBrowserManagedOnly(t *testing.T) {
	var installs []string
	old := installAgent
	installAgent = func(_ context.Context, _ backend.Backend, m *config.Machine, approve func() error) (agentbin.Paths, error) {
		if approve != nil {
			t.Errorf("%s: install gated behind approve on a managed box", m.Name)
		}
		installs = append(installs, m.Name)
		return agentbin.Paths{Agent: "/usr/local/bin/devvm-agent", Shim: backend.ManagedShimPath}, nil
	}
	t.Cleanup(func() { installAgent = old })

	for _, m := range []*config.Machine{managedSmol("web"), config.NewRemote("cloud", true, "dev@cloud")} {
		installs = nil
		rb := &recBackend{}
		if err := Browser(context.Background(), rb, m); err != nil {
			t.Fatal(err)
		}
		if strings.Join(installs, ",") != m.Name {
			t.Fatalf("%s: installs = %v", m.Name, installs)
		}
		if len(rb.runs) != 1 || rb.users[0] != "root" || rb.runs[0][0] != "sh" ||
			rb.runs[0][2] != writeProfileScript(backend.ManagedShimPath) {
			t.Fatalf("%s: runs = %q as %q, want the profile.d write as root", m.Name, rb.runs, rb.users)
		}
	}

	// A smol VM devvm did not create gets nothing (config.md §2), nor does
	// an adopt host (its agent waits for consent at auth, and it never
	// gets a profile.d write).
	for _, m := range []*config.Machine{{Name: "found", Backend: config.BackendSmol}, config.NewRemote("adopted", false, "me@adopted")} {
		installs = nil
		rb := &recBackend{}
		if err := Browser(context.Background(), rb, m); err != nil {
			t.Fatal(err)
		}
		if len(installs) != 0 || len(rb.runs) != 0 {
			t.Errorf("%s: installs %v, runs %q; want none", m.Name, installs, rb.runs)
		}
	}
}

// managedSmol is a smol record its discovery source settled as managed
// (devvm made it): what every smol path that installs or shapes assumes.
func managedSmol(name string) *config.Machine {
	m := &config.Machine{Name: name, Backend: config.BackendSmol}
	m.SetManaged(true)
	return m
}
