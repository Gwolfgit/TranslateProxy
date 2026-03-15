# TranslateProxy

A TLS-intercepting forward proxy that detects Cyrillic text in web responses and translates it to English on the fly.

Built with **Squid** (SSL bump) and a **Go ICAP service** that handles content inspection, translation, and caching.

## Architecture

```
                         ┌─────────────────────────────────────────────────┐
                         │                  Docker Compose                 │
                         │                                                 │
  Client ──► port 3128 ──┤  ┌───────────┐    ICAP    ┌──────────────┐     │
             (HTTPS)     │  │   Squid    │──────────►│  Go Translator│     │
                         │  │ (SSL Bump) │◄──────────│   (:1344)     │     │
                         │  └───────────┘            └──────┬───────┘     │
                         │        │                         │              │
                         │        │                    ┌────▼────┐         │
                         │   Corporate CA              │  Cache   │         │
                         │   re-encryption             │  Tiers   │         │
                         │                             └────┬────┘         │
                         └──────────────────────────────────┼──────────────┘
                                                            │
                                                   ┌───────▼────────┐
                                                   │ Google Translate│
                                                   │   (free API)   │
                                                   └────────────────┘
```

**Flow:** Client connects via HTTPS → Squid decrypts with SSL bump → response body sent to Go service via ICAP → Cyrillic detected and translated → modified response re-encrypted with corporate CA → delivered to client.

## Cache Architecture

Translations are cached in a two-tier system to minimize API calls. Text is normalized before hashing (Unicode NFC, collapsed whitespace, normalized quotes/dashes, stripped zero-width chars) so minor variations share the same cache entry.

```
Request → normalizeForCache() → FNV-64a hash
                                    │
                              ┌─────▼─────┐
                              │ Memory LRU │  ~µs lookup
                              │  (tier 1)  │  100k entries, in-process
                              └─────┬─────┘
                                 miss
                              ┌─────▼─────┐
                              │  BoltDB    │  persistent, 24h TTL
                              │  (tier 2)  │  survives restarts
                              └─────┬─────┘  promotes hits → memory
                                 miss
                              ┌─────▼─────┐
                              │ Google API │  batched, 10x concurrent
                              │  (tier 3)  │  stores → both tiers
                              └───────────┘
```

## Quick Start

```bash
# 1. Generate the corporate CA
./scripts/generate-ca.sh

# 2. Start the proxy
docker compose up -d

# 3. Trust the CA on your machine (see output of generate-ca.sh)

# 4. Use the proxy
curl -x http://localhost:3128 --cacert certs/ca.pem https://ru.wikipedia.org/wiki/Go
```

## Project Structure

```
├── docker-compose.yml          # Orchestrates squid + translator
├── Dockerfile.squid            # Ubuntu 24.04, squid-openssl, SSL bump
├── Dockerfile.translator       # Go 1.22 multi-stage build
├── squid/
│   ├── squid.conf              # TLS interception + ICAP routing
│   └── entrypoint.sh           # Cert setup, cache init, squid start
├── translator/
│   ├── main.go                 # ICAP handler, Cyrillic detection, response modification
│   ├── icap.go                 # ICAP protocol server (preview, chunked encoding)
│   ├── translate.go            # Batched concurrent Google Translate API client
│   ├── cache.go                # Tiered LRU + BoltDB cache with TTL
│   ├── normalize.go            # Text normalization for cache keys
│   └── go.mod
├── scripts/
│   ├── generate-ca.sh          # Corporate root CA (4096-bit RSA, 10yr)
│   └── generate-client-cert.sh # Per-client certs signed by CA
└── certs/                      # Generated certs (gitignored)
```

## Configuration

| Setting | Location | Default |
|---------|----------|---------|
| Proxy port | `docker-compose.yml` | 3128 |
| Memory cache size | `translator/main.go` | 100,000 entries |
| Disk cache TTL | `translator/main.go` | 24 hours |
| Batch concurrency | `translator/translate.go` | 10 parallel |
| Max query length | `translator/translate.go` | 4500 chars |
| CA validity | `scripts/generate-ca.sh` | 3650 days |

## Certificates

**Generate the corporate CA:**

```bash
./scripts/generate-ca.sh
```

Creates `certs/ca.pem` (distribute to clients), `certs/ca.key` (keep secret), and `certs/ca-combined.pem` (used by Squid internally).

**Trust on client machines:**

```bash
# Linux
sudo cp certs/ca.pem /usr/local/share/ca-certificates/corporate-proxy.crt
sudo update-ca-certificates

# macOS
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain certs/ca.pem

# Windows
certutil -addstore Root certs/ca.der
```

**Generate client certificates (optional):**

```bash
./scripts/generate-client-cert.sh workstation-01
```

## Logging

The translator logs every ICAP request with:

- Domain
- Content-Type and Content-Length
- Content-Encoding (gzip/deflate/br)
- Number of Cyrillic strings detected
- Number translated (and from which cache tier)
- Cache stats (memory entries, disk entries, hit rate)
- Total processing time

Example output:

```
[ICAP] === Request received ===
[ICAP]   Domain:         ru.wikipedia.org
[ICAP]   Content-Type:   text/html; charset=UTF-8
[ICAP]   Body size:      241803 bytes (decompressed)
[ICAP]   Cyrillic strings detected: 2406
[TRANSLATE] 1719 segments: 1719 cache hits, 0 to translate
[CACHE] mem=0 disk=1719 | mem_hits=0 disk_hits=1719 misses=0 hit_rate=100.0%
[ICAP]   Processing time: 409ms
[ICAP] === Request complete ===
```

## Performance

Tested on the Russian Wikipedia article for "Go" (~240KB, 2400+ Cyrillic segments):

| Scenario | API Calls | Translation Time | Total Time |
|----------|-----------|-----------------|------------|
| No cache (v1, sequential) | 1728 | 16.7s | ~17s |
| Batched + concurrent | 44 | 503ms | 1.1s |
| Memory cache hit | 0 | <1ms | 0.5s |
| Disk cache hit (after restart) | 0 | <1ms | 0.6s |
