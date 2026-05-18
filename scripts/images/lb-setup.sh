#!/bin/sh
# lb-setup.sh — LB-specific chroot setup
#
# Runs inside the Alpine image chroot after packages are installed.
# Sets up HAProxy placeholder config and lb-agent OpenRC init script.
# The lb-agent polls the AWS gateway for config updates and reports health
# via SigV4-signed HTTP requests. Credentials come from cloud-init env vars.

set -e

# Create haproxy config directory and placeholder config.
mkdir -p /etc/haproxy
cat > /etc/haproxy/haproxy.cfg <<'EOF'
# Placeholder config — replaced by lb-agent on first config fetch
global
    daemon
    maxconn 256

defaults
    mode http
    timeout connect 5s
    timeout client 30s
    timeout server 30s
EOF

# Create lb-agent OpenRC init script
mkdir -p /etc/init.d
cat > /etc/init.d/lb-agent <<'INITSCRIPT'
#!/sbin/openrc-run

description="LB Gateway Config Agent"
command="/usr/local/bin/lb-agent"
command_args="--lb-id=${LB_LB_ID:-unknown} --gateway=${LB_GATEWAY_URL} --access-key=${LB_ACCESS_KEY} --secret-key=${LB_SECRET_KEY}"
command_background=true
pidfile="/run/lb-agent.pid"
output_log="/var/log/lb-agent.log"
error_log="/var/log/lb-agent.log"

depend() {
    need net
    after firewall
}
INITSCRIPT
chmod 755 /etc/init.d/lb-agent

# Move networking from boot to default runlevel so cloud-init can write
# the network config before the networking service starts. Without this,
# Alpine's networking service races cloud-init and starts with no config.
rc-update del networking boot 2>/dev/null || true
rc-update add networking default

# Do NOT enable lb-agent at boot — cloud-init must write
# /etc/conf.d/lb-agent (with LB_LB_ID, LB_GATEWAY_URL, LB_ACCESS_KEY,
# LB_SECRET_KEY) before the service starts.
# Cloud-init runcmd starts the service after write_files.
