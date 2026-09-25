#!/bin/sh
# deploy.sh: build sbm-sync for Linux and install it on a server that runs
# sbm-sync.service (see sbm-sync.service) with systemd and nginx.
#
#     sh contrib/deploy.sh root@example.org
#
# The script keeps the old binary as /usr/local/bin/sbm-sync.bak-<time>,
# installs sfeed for the feed job, and starts sbm-feed.timer.
set -e
host=${1:?usage: sh contrib/deploy.sh user@host}
cd "$(dirname "$0")/.."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o sbm-sync-linux .
scp sbm-sync-linux contrib/sbm-feed.service contrib/sbm-feed.timer "$host:/tmp/"
rm sbm-sync-linux
ssh "$host" sh -s << 'EOF'
set -e
if ! command -v sfeed_update >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q sfeed
fi
if [ -f /usr/local/bin/sbm-sync ]; then
    cp -p /usr/local/bin/sbm-sync "/usr/local/bin/sbm-sync.bak-$(date +%Y%m%d-%H%M%S)"
fi
install -m 755 /tmp/sbm-sync-linux /usr/local/bin/sbm-sync
install -m 644 /tmp/sbm-feed.service /tmp/sbm-feed.timer /etc/systemd/system/
rm /tmp/sbm-sync-linux /tmp/sbm-feed.service /tmp/sbm-feed.timer
systemctl daemon-reload
systemctl restart sbm-sync
systemctl enable --now sbm-feed.timer
sleep 1
if ! systemctl is-active --quiet sbm-sync; then
    journalctl -u sbm-sync -n 20 --no-pager
    exit 1
fi
echo "sbm-sync: $(systemctl is-active sbm-sync), sbm-feed.timer: $(systemctl is-active sbm-feed.timer)"
systemctl list-timers sbm-feed.timer --no-pager
EOF
