// Package bootstrap holds the guest-side steps of `devvm setup` (config.md
// §3), in setup's order: the user devvm works as, devvm's own prereqs, the
// agent and BROWSER shim, ssh hardening (after setup's keys step), and the
// user's hook (a dotfiles bootstrap.sh is just one instance; any url:/cmd:
// works). The package keeps its old name; the verb is `setup`.
package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// devvmPackages are what devvm itself needs in a box it made, beyond
// the base image: build tools, curl/git, plus tmux (attach's dev session),
// gh (repos' owner/repo clones) and mosh-server (the mosh transport). Those
// last three used to come from the hook, which is why attach preflights
// tmux (RequireGuestTmux); setup installs them now (config.md §3).
const devvmPackages = "build-essential ca-certificates curl git rsync tmux mosh"

// aptGet is devvm's own apt-get for installs: it waits up to five minutes
// for the dpkg lock instead of failing at once. A fresh cloud VM runs
// apt-daily and unattended-upgrades from boot, and harden enables the
// latter, so devvm's apt can meet another apt that holds the lock.
const aptGet = "apt-get -o DPkg::Lock::Timeout=300"

// aptUpdateFn defines apt_update, devvm's own `apt-get update`: the lock
// timeout does not cover update's lists lock (it fails at once, rc 100,
// while apt-daily holds it), so update is retried every 5s, for up to five
// minutes. Each script that updates defines it once, after its set -e.
const aptUpdateFn = `
apt_update() {
    tries=0
    until apt-get update; do
        tries=$((tries + 1))
        [ "$tries" -lt 60 ] || return 1
        sleep 5
    done
}
`

// installGH installs gh from the distro when it has it (Debian 12+, Ubuntu
// 22.04+) and from GitHub's own apt repository otherwise. Idempotent.
const installGH = `
if ! command -v gh >/dev/null 2>&1; then
    if ! DEBIAN_FRONTEND=noninteractive ` + aptGet + ` install -y gh; then
        install -d -m 0755 /etc/apt/keyrings
        curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
            -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
        chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
        printf 'deb [arch=%s signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main\n' \
            "$(dpkg --print-architecture)" >/etc/apt/sources.list.d/github-cli.list
        apt_update
        DEBIAN_FRONTEND=noninteractive ` + aptGet + ` install -y gh
    fi
fi
`

// genLocales generates the locales named after the script's first argument
// ("${@:2}": the host's own, HostLocales). ssh sends LANG and LC_* to the
// box (SendEnv in the stock ssh_config, AcceptEnv in Ubuntu's sshd), and a
// cloud image has only C.UTF-8 and en_US.UTF-8, so a host in en_CA.UTF-8 got
// "setlocale: LC_ALL: cannot change locale" from every bash and perl there.
// /etc/locale.gen is edited where it exists (Debian, Ubuntu 22.04+: a bare
// locale-gen reads it), else the name goes to locale-gen (older Ubuntu). A
// failure here is one warning, never a failed setup. Both prereq scripts run
// it right after apt_update (it may install locales) and before the package
// install, so the apt and perl calls after it do not warn on a first setup.
// locale-gen itself runs under LC_ALL=C: the session's LC_ALL names the
// locale it is about to make, and it would warn about that once. Through
// env, not a bash prefix assignment: bash restores a prefixed LC_ALL in
// this shell after the command and warns then ("environment: line N"),
// since glibc does not re-read the archive locale-gen just rewrote.
const genLocales = `
gen_locales() {
    gen=
    for loc in "$@"; do
        locale -a 2>/dev/null | grep -qix "${loc%UTF-8}utf8" && continue
        command -v locale-gen >/dev/null 2>&1 || \
            DEBIAN_FRONTEND=noninteractive ` + aptGet + ` install -y locales || return 1
        grep -qx "$loc UTF-8" /usr/share/i18n/SUPPORTED || { echo "devvm: this image has no locale $loc; skipping it" >&2; continue; }
        if [ ! -e /etc/locale.gen ]; then
            env LC_ALL=C locale-gen "$loc" || return 1
            continue
        fi
        if grep -q "^# *$loc UTF-8" /etc/locale.gen; then
            sed -i "s/^# *\($loc UTF-8\)/\1/" /etc/locale.gen
        elif ! grep -qx "$loc UTF-8" /etc/locale.gen; then
            printf '%s UTF-8\n' "$loc" >>/etc/locale.gen
        fi
        gen=1
    done
    [ -z "$gen" ] || env LC_ALL=C locale-gen
}
gen_locales "${@:2}" || echo "devvm: could not generate this host's locale(s): ${*:2}" >&2
`

// localeRe is a locale name HostLocales passes on: language_TERRITORY in
// UTF-8, nothing else (C, POSIX and C.UTF-8 need no generating, and the
// name lands in a sed and grep pattern in the guest).
var localeRe = regexp.MustCompile(`^([a-z]{2,3}_[A-Z]{2})\.(?i:utf-?8)$`)

// HostLocales is every locale this host's environment names (LANG, LC_ALL,
// LC_*), as xx_YY.UTF-8, sorted: what ssh will send to the box.
func HostLocales(environ []string) []string {
	seen := map[string]bool{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if k != "LANG" && !strings.HasPrefix(k, "LC_") {
			continue
		}
		if m := localeRe.FindStringSubmatch(v); m != nil {
			seen[m[1]+".UTF-8"] = true
		}
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// smolPrereqs installs devvm's packages, writes hostname/hosts, and the
// gai.conf IPv4-precedence fix. Runs as root in the guest ($1 is the
// machine name), after EnsureUser. Ports smol_prepare_guest.
const smolPrereqs = `
set -e
` + aptUpdateFn + `
apt_update
` + genLocales + `
DEBIAN_FRONTEND=noninteractive ` + aptGet + ` install -y ` + devvmPackages + `
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

// ensureUserScript creates the unprivileged user ($1, always dev) devvm works as, with
// NOPASSWD sudo, and copies root's authorized_keys over so the client key
// that reached root keeps working as that user. Runs as root with no sudo
// wrap: sudo may not be installed yet (a fresh cloud VM, a smol base
// image), so it installs it here. Idempotent; an existing user keeps its
// home and keys. The sudoers drop-in is /etc/sudoers.d/devvm-USER.
const ensureUserScript = `
set -e
u="$1"
export DEBIAN_FRONTEND=noninteractive
` + aptUpdateFn + `
command -v sudo >/dev/null 2>&1 || { apt_update; ` + aptGet + ` install -y sudo; }
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
// smol backend's `sudo -u dev`) relies on: config.DefaultUser exists,
// unprivileged, with NOPASSWD sudo and root's keys. asRoot is how this backend runs a command
// as root without sudo: on smol ExecOpts{User: "root"} (the exec enters as
// root); on a remote box reached as root@host, the zero ExecOpts (the login
// is root already, and a fresh box may have no sudo to wrap with).
func EnsureUser(ctx context.Context, b backend.Backend, asRoot backend.ExecOpts) error {
	asRoot.Stream = true
	if err := b.Run(ctx, asRoot, "bash", "-c", ensureUserScript, "_", config.DefaultUser); err != nil {
		return fmt.Errorf("creating user %s: %w", config.DefaultUser, err)
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

// remotePrereqs is the install path for a provider server devvm made (the
// user + sudo exist: pre-existing, or created by EnsureUser on a root-only
// box). Ports ssh_prepare_guest.
const remotePrereqs = `
set -e
` + aptUpdateFn + `
apt_update
` + genLocales + `
DEBIAN_FRONTEND=noninteractive ` + aptGet + ` install -y ` + devvmPackages + `
` + installGH

// Prereqs installs the minimal packages as root, on a box devvm made (a smol
// VM, a provider server): setup's one caller has refused anything else.
func Prereqs(ctx context.Context, b backend.Backend, m *config.Machine) error {
	script := remotePrereqs
	if !m.IsRemote() {
		script = smolPrereqs
	}
	argv := append([]string{"bash", "-c", script, "_", m.Name}, HostLocales(os.Environ())...)
	return b.Run(ctx, backend.ExecOpts{User: "root", Stream: true}, argv...)
}
