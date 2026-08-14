// Package provision runs the guest-side provisioning devvm owns: the built-in
// minimal prereqs (a ready box), a pluggable provisioner (bootstrap.sh is just
// one instance), and ssh hardening. This decouples devvm from bootstrap.sh —
// the default provisioner reproduces the old curl|bash path, but any URL/cmd
// works.
package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// smolPrereqs installs the minimal packages and the dev user, writes the agent
// marker, hostname/hosts, and the gai.conf IPv4-precedence fix. Runs as root in
// the guest ($1 is the machine name). Ports smol_prepare_guest.
const smolPrereqs = `
set -e
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y \
    build-essential ca-certificates curl git sudo
if ! id -u dev >/dev/null 2>&1; then
    useradd --create-home --shell /bin/bash dev
fi
touch /etc/devvm-agent
printf '%s ALL=(ALL) NOPASSWD:ALL\n' dev >/etc/sudoers.d/devvm
chmod 0440 /etc/sudoers.d/devvm
printf '%s\n' "$1" >/etc/hostname
sed -i '/^127\.0\.1\.1[[:space:]]/d' /etc/hosts
grep -qE "[[:space:]]$1([[:space:]]|\$)" /etc/hosts || printf '127.0.1.1 %s\n' "$1" >>/etc/hosts
# smol's macOS NAT is IPv4-only: guests get an IPv6 SLAAC address with no working
# route, so AAAA-first hosts (e.g. api.anthropic.com) reset connections from Node
# tools like Claude Code that don't fall back to v4. Bump IPv4-mapped precedence
# so getaddrinfo prefers IPv4.
grep -qs '^precedence ::ffff:0:0/96  100' /etc/gai.conf || \
    printf 'precedence ::ffff:0:0/96  100\n' >>/etc/gai.conf
`

// ensureDevUser creates the unprivileged dev user on a remote-managed host whose
// ssh login is root (a fresh cloud VM). Mirrors smolPrereqs' user setup and
// copies root's authorized_keys over so the client key that reached root keeps
// working as dev. Runs as the login user (root) with no sudo wrap — sudo may not
// be installed yet, so it installs it here.
const ensureDevUserScript = `
set -e
export DEBIAN_FRONTEND=noninteractive
command -v sudo >/dev/null 2>&1 || { apt-get update; apt-get install -y sudo; }
if ! id -u dev >/dev/null 2>&1; then
    useradd --create-home --shell /bin/bash dev
fi
printf '%s ALL=(ALL) NOPASSWD:ALL\n' dev >/etc/sudoers.d/devvm
chmod 0440 /etc/sudoers.d/devvm
install -d -m 700 -o dev -g dev /home/dev/.ssh
if [ -s /root/.ssh/authorized_keys ] && [ ! -s /home/dev/.ssh/authorized_keys ]; then
    install -m 600 -o dev -g dev /root/.ssh/authorized_keys /home/dev/.ssh/authorized_keys
fi
`

// LoginUser reports who the backend's default (non-root) exec actually runs as
// on the guest.
func LoginUser(ctx context.Context, b backend.Backend) (string, error) {
	var out bytes.Buffer
	if err := b.Run(ctx, backend.ExecOpts{Stdout: &out}, "id", "-un"); err != nil {
		return "", fmt.Errorf("checking remote login user: %w", err)
	}
	return strings.TrimSpace(out.String()), nil
}

// EnsureDevUser establishes the invariant the ssh backend's rootWrap relies on —
// the login user is an unprivileged dev user with NOPASSWD sudo — on a
// remote-managed host currently reached as root (a fresh cloud VM). It reports
// whether it created the user, i.e. whether the caller must switch the machine's
// ssh_host to the dev user. No-op on other backends or non-root logins.
func EnsureDevUser(ctx context.Context, b backend.Backend, m *config.Machine) (bool, error) {
	if m.Backend != config.BackendRemoteManaged {
		return false, nil
	}
	user, err := LoginUser(ctx, b)
	if err != nil {
		return false, err
	}
	if user != "root" {
		return false, nil
	}
	// Deliberately not ExecOpts{User: "root"}: that would prefix sudo, which a
	// fresh box may lack. The login user is already root.
	if err := b.Run(ctx, backend.ExecOpts{Stream: true}, "bash", "-c", ensureDevUserScript); err != nil {
		return false, fmt.Errorf("creating dev user: %w", err)
	}
	return true, nil
}

// SwitchUser rewrites the user part of an ssh destination ("root@h" → "dev@h").
// A bare host or ssh-config alias gets the user prepended — a command-line
// user@ overrides an alias's configured User, so this holds for aliases too.
func SwitchUser(sshHost, user string) string {
	host := sshHost
	if _, h, ok := strings.Cut(sshHost, "@"); ok {
		host = h
	}
	return user + "@" + host
}

// managedRemotePrereqs is the lighter install path for remote-managed hosts (the
// user + sudo exist — pre-existing, or created by EnsureDevUser on a root-only
// box). Ports ssh_prepare_guest.
const managedRemotePrereqs = `
set -e
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y \
    build-essential ca-certificates curl git rsync
touch /etc/devvm-agent
`

// checkPrereqs verifies (never installs) that an adopted remote-unmanaged host
// has what devvm needs. Read-only and non-root: we don't shape a host we don't
// own. Missing tools only warn — everything but `attach` works without tmux, and
// the user may have equivalents installed elsewhere.
const checkPrereqs = `
missing=
for c in bash git rsync tmux; do
    command -v "$c" >/dev/null 2>&1 || missing="$missing $c"
done
if [ -n "$missing" ]; then
    printf 'devvm: %s: recommended tools not found:%s (install them if you hit trouble)\n' \
        "$1" "$missing" >&2
fi
`

// Prereqs prepares the guest: managed backends (smol, remote-managed) install the
// minimal packages as root; adopted remote-unmanaged hosts are only checked.
func Prereqs(ctx context.Context, b backend.Backend, m *config.Machine) error {
	if !m.Managed() {
		// `bash -lc` (a login shell) so brew-installed tools on PATH count as
		// present; the argv is the shell itself, so no ExecOpts.Login wrap.
		return b.Run(ctx, backend.ExecOpts{Stream: true},
			"bash", "-lc", checkPrereqs, "_", m.Name)
	}
	script := managedRemotePrereqs
	if m.Backend == config.BackendSmol {
		script = smolPrereqs
	}
	return b.Run(ctx, backend.ExecOpts{User: "root", Stream: true},
		"bash", "-c", script, "_", m.Name)
}
