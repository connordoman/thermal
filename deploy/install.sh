#!/bin/sh
# Installs thermal on a Raspberry Pi (or any Linux machine) and keeps it up
# to date. Run it once on the Pi; running it again updates the setup:
#
#   curl -fsSL https://raw.githubusercontent.com/connordoman/thermal/main/deploy/install.sh | sudo sh
#
# It installs Docker if needed, puts compose.yaml and .env in /opt/thermal,
# and adds a systemd timer that pulls the published image every 5 minutes
# and restarts the server only when the image has changed.
#
# Choose the channel with THERMAL_TAG in /opt/thermal/.env: latest (tagged
# releases, the default), edge (every push to main) or a fixed version
# such as 0.2.0.
set -eu

repo=connordoman/thermal
dir=/opt/thermal
raw="https://raw.githubusercontent.com/$repo/${THERMAL_REF:-main}"

if [ "$(id -u)" -ne 0 ]; then
	echo "Run as root: curl -fsSL $raw/deploy/install.sh | sudo sh" >&2
	exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
	echo "==> Installing Docker"
	curl -fsSL https://get.docker.com | sh
fi
if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
	usermod -aG docker "$SUDO_USER" # docker without sudo, from the next login
fi

echo "==> Writing $dir"
mkdir -p "$dir"
curl -fsSL -o "$dir/compose.yaml" "$raw/compose.yaml"
if [ ! -f "$dir/.env" ]; then
	cat >"$dir/.env" <<'ENV'
# Image channel: latest (releases), edge (main) or a version like 0.2.0.
THERMAL_TAG=latest
# Settings for the server go here too; see the README's Configuration.
ENV
	chmod 600 "$dir/.env"
fi

echo "==> Installing the update timer"
cat >/etc/systemd/system/thermal-update.service <<UNIT
[Unit]
Description=Update the thermal print server
Wants=network-online.target docker.service
After=network-online.target docker.service

[Service]
Type=oneshot
WorkingDirectory=$dir
ExecStart=/usr/bin/docker compose pull --quiet
ExecStart=/usr/bin/docker compose up --detach --remove-orphans
ExecStart=/usr/bin/docker image prune --force
UNIT
cat >/etc/systemd/system/thermal-update.timer <<'UNIT'
[Unit]
Description=Check for thermal updates every 5 minutes

[Timer]
OnBootSec=1min
OnUnitActiveSec=5min
RandomizedDelaySec=30s

[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now docker.service thermal-update.timer

echo "==> Starting thermal"
systemctl start thermal-update.service
sleep 3
docker compose --project-directory "$dir" logs thermal | grep -A3 -i bootstrap || true

cat <<DONE

thermal is running on port 8080 and updates itself from ghcr.io/$repo.
  Settings:  $dir/.env  (then: sudo systemctl start thermal-update)
  Logs:      sudo docker compose --project-directory $dir logs -f
  Updates:   systemctl list-timers thermal-update; journalctl -u thermal-update
DONE
