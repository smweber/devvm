package bootstrap

import (
	"context"
	"fmt"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// hardenBody is setup's hardening script (ufw + sshd drop-in + auto-updates),
// idempotent and additive: safe on every setup re-run. It
// reads SSH_PORT and WANT_FAIL2BAN prepended by Harden and takes the dev user
// as $1. Preserves the two fail-safes from harden_machine: refuse if
// authorized_keys is empty (would lock you out) and validate sshd config before
// reloading. Runs as root.
const hardenBody = `
set -euo pipefail
DEV_USER="$1"
AUTHKEYS="$(getent passwd "$DEV_USER" | cut -d: -f6)/.ssh/authorized_keys"

# Fail-safe: never disable password auth without a working key in place.
if [ ! -s "$AUTHKEYS" ]; then
    echo "refusing to harden: $AUTHKEYS is empty (would lock you out)" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive
` + aptUpdateFn + `
apt_update
` + aptGet + ` install -y ufw unattended-upgrades${WANT_FAIL2BAN:+ fail2ban}

# Firewall: default-deny inbound; SSH is the only public TCP port devvm
# opens. Everything else is reached over the SSH -L tunnel. mosh-server (a
# prereq) listens on UDP 60000-61000, so that range is open too, or the mosh
# transport could not reach a hardened box. Additive, never a reset: every
# setup re-runs this, and a reset would drop rules the user or the hook
# added (and leave six /etc/ufw/*.rules.TIMESTAMP backups per run). Each
# command below is idempotent on its own.
ufw default deny incoming
ufw default allow outgoing
ufw allow "${SSH_PORT}/tcp"
ufw allow 60000:61000/udp
ufw --force enable

# sshd hardening as a drop-in, validated before reload so we can't lock out.
cat >/etc/ssh/sshd_config.d/10-devvm.conf <<SSHD
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
PubkeyAuthentication yes
AllowUsers $DEV_USER
X11Forwarding no
SSHD

if sshd -t; then
    systemctl reload ssh 2>/dev/null || systemctl reload sshd
else
    echo "sshd config invalid; removing drop-in" >&2
    rm -f /etc/ssh/sshd_config.d/10-devvm.conf
    exit 1
fi

# Automatic security updates.
dpkg-reconfigure -f noninteractive unattended-upgrades || true
systemctl enable --now unattended-upgrades 2>/dev/null || true

[ -n "$WANT_FAIL2BAN" ] && systemctl enable --now fail2ban 2>/dev/null || true

echo "devvm: hardening applied (firewall + sshd + auto-updates)"
`

// Harden applies firewall + sshd hardening + auto-updates to an ssh machine,
// with fail2ban when asked (a template's fail2ban: the entry holds no
// shaping fields).
func Harden(ctx context.Context, b backend.Backend, m *config.Machine, withFail2ban bool) error {
	// Pin AllowUsers to the actual connection user rather than guessing from
	// SUDO_USER/logname inside the script. Hardening a root login would write
	// `PermitRootLogin no` + `AllowUsers root` — a guaranteed lockout.
	user, err := LoginUser(ctx, b)
	if err != nil {
		return err
	}
	if user == "" || user == "root" {
		return fmt.Errorf("refusing to harden: the connection user is root; run 'devvm setup %s' to create the user first", m.Name)
	}
	fail2ban := ""
	if withFail2ban {
		fail2ban = "1"
	}
	port := m.SSHPort
	if port == 0 {
		port = 22 // the ssh default (a provider record says 22)
	}
	script := fmt.Sprintf("SSH_PORT=%d\nWANT_FAIL2BAN=%s\n%s", port, fail2ban, hardenBody)
	return b.Run(ctx, backend.ExecOpts{User: "root", Stream: true}, "bash", "-c", script, "_", user)
}
