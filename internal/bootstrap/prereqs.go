// Package provision runs the guest-side provisioning devvm owns: the built-in
// minimal prereqs (a ready box), a pluggable provisioner (bootstrap.sh is just
// one instance), and ssh hardening. This decouples devvm from bootstrap.sh —
// the default provisioner reproduces the old curl|bash path, but any URL/cmd
// works.
package bootstrap

import (
	"context"

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

// managedRemotePrereqs is the lighter install path for remote-managed hosts (the
// user + sudo already exist). Ports ssh_prepare_guest.
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
