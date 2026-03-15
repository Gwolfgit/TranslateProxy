#!/usr/bin/env bash
#
# generate-client-cert.sh — Generate a client certificate signed by the corporate CA.
# Usage: ./generate-client-cert.sh <client-name>
# Outputs: certs/<client-name>.key, certs/<client-name>.pem, certs/<client-name>.p12
#
set -euo pipefail

if [ $# -lt 1 ]; then
    echo "Usage: $0 <client-name> [days]"
    echo "Example: $0 workstation-01"
    exit 1
fi

CLIENT_NAME="$1"
CLIENT_DAYS="${2:-825}"
CERTS_DIR="$(cd "$(dirname "$0")/../certs" && pwd)"
CA_KEY="$CERTS_DIR/ca.key"
CA_CERT="$CERTS_DIR/ca.pem"

if [ ! -f "$CA_KEY" ] || [ ! -f "$CA_CERT" ]; then
    echo "Error: CA certificate not found. Run generate-ca.sh first."
    exit 1
fi

echo "==> Generating client private key for '$CLIENT_NAME'..."
openssl genrsa -out "$CERTS_DIR/${CLIENT_NAME}.key" 2048

echo "==> Generating CSR..."
openssl req -new \
    -key "$CERTS_DIR/${CLIENT_NAME}.key" \
    -out "$CERTS_DIR/${CLIENT_NAME}.csr" \
    -subj "/C=US/ST=State/L=City/O=Corporate/OU=Clients/CN=${CLIENT_NAME}"

echo "==> Signing certificate with CA (valid ${CLIENT_DAYS} days)..."
openssl x509 -req \
    -in "$CERTS_DIR/${CLIENT_NAME}.csr" \
    -CA "$CA_CERT" \
    -CAkey "$CA_KEY" \
    -CAcreateserial \
    -out "$CERTS_DIR/${CLIENT_NAME}.pem" \
    -days "$CLIENT_DAYS" \
    -sha256

echo "==> Creating PKCS12 bundle..."
openssl pkcs12 -export \
    -in "$CERTS_DIR/${CLIENT_NAME}.pem" \
    -inkey "$CERTS_DIR/${CLIENT_NAME}.key" \
    -certfile "$CA_CERT" \
    -out "$CERTS_DIR/${CLIENT_NAME}.p12" \
    -passout pass:changeit \
    -name "$CLIENT_NAME"

rm -f "$CERTS_DIR/${CLIENT_NAME}.csr"

echo ""
echo "Done. Client certificate files:"
echo "  ${CLIENT_NAME}.key  — Client private key"
echo "  ${CLIENT_NAME}.pem  — Client certificate"
echo "  ${CLIENT_NAME}.p12  — PKCS12 bundle (password: changeit)"
