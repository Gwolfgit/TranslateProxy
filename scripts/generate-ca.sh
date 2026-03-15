#!/usr/bin/env bash
#
# generate-ca.sh — Generate a corporate root CA for TLS interception.
# Outputs: certs/ca.pem  (certificate)
#          certs/ca.key  (private key)
#          certs/ca-combined.pem  (cert + key, for Squid)
#
set -euo pipefail

CERTS_DIR="$(cd "$(dirname "$0")/../certs" && pwd)"
CA_DAYS="${CA_DAYS:-3650}"
CA_SUBJECT="${CA_SUBJECT:-/C=US/ST=State/L=City/O=Corporate/OU=IT/CN=Corporate Proxy CA}"

mkdir -p "$CERTS_DIR"

echo "==> Generating CA private key..."
openssl genrsa -out "$CERTS_DIR/ca.key" 4096

echo "==> Generating CA certificate (valid ${CA_DAYS} days)..."
openssl req -new -x509 \
    -key "$CERTS_DIR/ca.key" \
    -out "$CERTS_DIR/ca.pem" \
    -days "$CA_DAYS" \
    -subj "$CA_SUBJECT" \
    -sha256

echo "==> Creating combined PEM for Squid..."
cat "$CERTS_DIR/ca.pem" "$CERTS_DIR/ca.key" > "$CERTS_DIR/ca-combined.pem"
chmod 600 "$CERTS_DIR/ca.key" "$CERTS_DIR/ca-combined.pem"

echo "==> Generating DER format for easy client import..."
openssl x509 -in "$CERTS_DIR/ca.pem" -outform DER -out "$CERTS_DIR/ca.der"

echo ""
echo "Done. Files created in $CERTS_DIR:"
echo "  ca.key           — CA private key (keep secret)"
echo "  ca.pem           — CA certificate (distribute to clients)"
echo "  ca-combined.pem  — Combined cert+key (used by Squid)"
echo "  ca.der           — DER format cert (for Windows/macOS import)"
echo ""
echo "To trust this CA on a client machine:"
echo "  Linux:   sudo cp $CERTS_DIR/ca.pem /usr/local/share/ca-certificates/corporate-proxy.crt && sudo update-ca-certificates"
echo "  macOS:   sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain $CERTS_DIR/ca.pem"
echo "  Windows: certutil -addstore Root $CERTS_DIR/ca.der"
