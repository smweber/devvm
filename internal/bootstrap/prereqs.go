// Package bootstrap holds the guest-side steps of `devvm setup` (config.md
// §3): the user devvm works as, devvm's own prereqs, the agent and BROWSER
// shim, the user's hook (a dotfiles bootstrap.sh is just one instance; any
// url:/cmd: works), and ssh hardening. The package keeps its old name; the
// verb is `setup`.
package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// devvmPackages are what devvm itself needs in a box it manages, beyond
// the base image: build tools, curl/git, plus tmux (attach's dev session),
// gh (repos' owner/repo clones) and mosh-server (the mosh transport). Those
// last three used to come from the hook, which is why attach preflights
// tmux (RequireGuestTmux); setup installs them now (config.md §3).
const devvmPackages = "build-essential ca-certificates curl git rsync tmux mosh"

// installGH installs gh from the distro when it has it (Debian 12+, Ubuntu
// 22.04+) and from GitHub's own apt repository otherwise. Idempotent.
const installGH = `
if ! command -v gh >/dev/null 2>&1; then
    if ! DEBIAN_FRONTEND=noninteractive apt-get install -y gh; then
        install -d -m 0755 /etc/apt/keyrings
        curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
            -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
        chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
        printf 'deb [arch=%s signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main\n' \
            "$(dpkg --print-architecture)" >/etc/apt/sources.list.d/github-cli.list
        apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y gh
    fi
fi
`

// smolPrereqs installs devvm's packages, writes hostname/hosts, and the
// gai.conf IPv4-precedence fix. Runs as root in the guest ($1 is the
// machine name), after EnsureUser. Ports smol_prepare_guest.
const smolPrereqs = `
set -e
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y ` + devvmPackages + `
` + installGH + `
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

// ensureUserScript creates the unprivileged user ($1) devvm works as, with
// NOPASSWD sudo, and copies root's authorized_keys over so the client key
// that reached root keeps working as that user. Runs as root with no sudo
// wrap: sudo may not be installed yet (a fresh cloud VM, a smol base
// image), so it installs it here. Idempotent; an existing user keeps its
// home and keys. The sudoers drop-in is per user (devvm-USER), so a user an
// earlier setup made (an older devvm wrote /etc/sudoers.d/devvm for dev)
// keeps its sudo when a template names another.
const ensureUserScript = `
set -e
u="$1"
export DEBIAN_FRONTEND=noninteractive
command -v sudo >/dev/null 2>&1 || { apt-get update; apt-get install -y sudo; }
if ! id -u "$u" >/dev/null 2>&1; then
    useradd --create-home --shell /bin/bash "$u"
fi
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$u" >"/etc/sudoers.d/devvm-$u"
chmod 0440 "/etc/sudoers.d/devvm-$u"
home="$(getent passwd "$u" | cut -d: -f6)"
[ -n "$home" ] || { echo "devvm: user $u has no home directory in passwd" >&2; exit 1; }
g="$(id -gn "$u")"
install -d -m 700 -o "$u" -g "$g" "$home/.ssh"
if [ -s /root/.ssh/authorized_keys ] && [ ! -s "$home/.ssh/authorized_keys" ]; then
    install -m 600 -o "$u" -g "$g" /root/.ssh/authorized_keys "$home/.ssh/authorized_keys"
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

// EnsureUser establishes the invariant the ssh backend's rootWrap (and the
// smol backend's `sudo -u dev`) relies on: user exists, unprivileged, with
// NOPASSWD sudo and root's keys. asRoot is how this backend runs a command
// as root without sudo: on smol ExecOpts{User: "root"} (the exec enters as
// root); on a remote box reached as root@host, the zero ExecOpts (the login
// is root already, and a fresh box may have no sudo to wrap with).
func EnsureUser(ctx context.Context, b backend.Backend, asRoot backend.ExecOpts, user string) error {
	if err := config.ValidUser(user); err != nil {
		return err
	}
	asRoot.Stream = true
	if err := b.Run(ctx, asRoot, "bash", "-c", ensureUserScript, "_", user); err != nil {
		return fmt.Errorf("creating user %s: %w", user, err)
	}
	return nil
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

// managedRemotePrereqs is the install path for managed remote boxes (the
// user + sudo exist: pre-existing, or created by EnsureUser on a root-only
// box). Ports ssh_prepare_guest.
const managedRemotePrereqs = `
set -e
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y ` + devvmPackages + `
` + installGH

// checkPrereqs verifies (never installs) that an adopted (unmanaged) remote host
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

// Prereqs prepares the guest: managed boxes (smol, managed remote) install the
// minimal packages as root; adopted remote hosts are only checked.
func Prereqs(ctx context.Context, b backend.Backend, m *config.Machine) error {
	if !m.Managed() {
		// `bash -lc` (a login shell) so brew-installed tools on PATH count as
		// present; the argv is the shell itself, so no ExecOpts.Login wrap.
		return b.Run(ctx, backend.ExecOpts{Stream: true},
			"bash", "-lc", checkPrereqs, "_", m.Name)
	}
	script := managedRemotePrereqs
	if !m.IsRemote() {
		script = smolPrereqs
	}
	return b.Run(ctx, backend.ExecOpts{User: "root", Stream: true},
		"bash", "-c", script, "_", m.Name)
}
