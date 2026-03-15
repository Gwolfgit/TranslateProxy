#!/usr/bin/env bash
set -e

# Copy mounted certs into squid's cert directory
if [ -f /mnt/certs/ca-combined.pem ]; then
    cp /mnt/certs/ca-combined.pem /etc/squid/certs/ca-combined.pem
    chown proxy:proxy /etc/squid/certs/ca-combined.pem
    chmod 600 /etc/squid/certs/ca-combined.pem
else
    echo "ERROR: /mnt/certs/ca-combined.pem not found!"
    echo "Run ./scripts/generate-ca.sh first."
    exit 1
fi

# Re-initialize SSL cert database if needed
if [ ! -d /var/lib/squid/ssl_db/certs ]; then
    /usr/lib/squid/security_file_certgen -c -s /var/lib/squid/ssl_db -M 4MB
    chown -R proxy:proxy /var/lib/squid/ssl_db
fi

# Initialize squid cache dirs
squid -z --foreground 2>/dev/null || true

echo "Starting Squid..."
exec squid --foreground -YC
