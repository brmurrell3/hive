#!/bin/bash
# Install hive-agent on a Raspberry Pi.
# Run as root: sudo ./install.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "=== Installing hive-agent ==="

# Copy binary
cp "${SCRIPT_DIR}/hive-agent" /usr/local/bin/hive-agent
chmod +x /usr/local/bin/hive-agent

# Create systemd service
cat > /etc/systemd/system/hive-agent.service << 'EOF'
[Unit]
Description=Hive Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-/etc/hive/agent.env
ExecStart=/usr/local/bin/hive-agent join \
    --token ${HIVE_TOKEN} \
    --control-plane ${HIVE_CONTROL_PLANE} \
    --agent-id ${HIVE_AGENT_ID}
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

# Create config directory
mkdir -p /etc/hive
mkdir -p /var/lib/hive/workspace

# Create default env file if it doesn't exist
if [ ! -f /etc/hive/agent.env ]; then
    cat > /etc/hive/agent.env << 'EOF'
# Hive Agent Configuration
# Edit these values and restart hive-agent:
#   sudo systemctl restart hive-agent

HIVE_TOKEN=
HIVE_CONTROL_PLANE=
HIVE_AGENT_ID=
EOF
    echo "Created /etc/hive/agent.env - edit this file with your join token and control plane address"
fi

# Reload and enable
systemctl daemon-reload
systemctl enable hive-agent

echo "=== Installation complete ==="
echo ""
echo "Configure the agent by editing /etc/hive/agent.env"
echo "Then start with: sudo systemctl start hive-agent"
