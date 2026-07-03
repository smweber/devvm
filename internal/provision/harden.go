package provision

import (
	"context"
	"fmt"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// hardenBody is the lockdown script (ufw + sshd drop-in + auto-updates). It
// reads SSH_PORT and WANT_FAIL2BAN prepended by Harden. Preserves the two
// fail-safes from harden_machine: refuse if authorized_keys is empty (would lock
// you out) and validate sshd config before reloading. Runs as root.
const hardenBody = `
set -euo pipefail
DEV_USER="${SUDO_USER:-$(logname 2>/dev/null || echo dev)}"
AUTHKEYS="/home/$DEV_USER/.ssh/authorized_keys"

# Fail-safe: never disable password auth without a working key in place.
if [ ! -s "$AUTHKEYS" ]; then
    echo "refusing to harden: $AUTHKEYS is empty (would lock you out)" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y ufw unattended-upgrades${WANT_FAIL2BAN:+ fail2ban}

# Firewall: default-deny inbound; SSH is the only public port. Everything else is
# reached over the SSH -L tunnel.
ufw --force reset
ufw default deny incoming
ufw default allow outgoing
ufw allow "${SSH_PORT}/tcp"
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

echo "devvm: lockdown applied (firewall + sshd + auto-updates)"
`

// Harden applies firewall + sshd hardening + auto-updates to an ssh machine.
func Harden(ctx context.Context, b backend.Backend, m *config.Machine) error {
	fail2ban := ""
	if m.Fail2ban {
		fail2ban = "1"
	}
	script := fmt.Sprintf("SSH_PORT=%d\nWANT_FAIL2BAN=%s\n%s", m.SSHPort, fail2ban, hardenBody)
	return b.Run(ctx, backend.ExecOpts{User: "root", Stream: true}, "bash", "-c", script)
}
