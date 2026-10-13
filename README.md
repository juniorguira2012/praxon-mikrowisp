# mikrowisp-proxy

Lightweight HTTP proxy that fronts a Mikrowisp ISP management installation. Forwards every API call to upstream and, for `GetInvoices` and `GetClientsDetails`, can drop rows from the response based on per-customer "extra" fields configured in a TOML file.

## What it does

The Mikrowisp panel lets the operator define arbitrary custom fields per customer (`tipo_cliente`, `etiqueta`, `rnc_factura`, etc.). This proxy lets the operator decide that customers matching certain values are not part of the data flow that goes downstream into a billing system.

For example, a rule like:

```toml
[[filter.exclude]]
field = "tipo_cliente"
value = "informal"
```

makes every customer Mikrowisp tags as `tipo_cliente=informal` invisible to the downstream caller. Their invoices never appear in `GetInvoices` and their record never appears in `GetClientsDetails`.

Every other Mikrowisp endpoint (`/api/v1/Ping`, `/api/v1/PaidInvoice`, etc.) is forwarded unchanged.

## Why it exists

Some downstream billing systems (e.g. Axentra ERP) are required by tax law to issue an electronic comprobante for every customer they see. The proxy gives the operator a place to filter customers OUT of the data flow BEFORE the downstream system ever sees them — so the downstream stays compliant by only ever seeing the customers that should be in scope.

Running this proxy is the operator's choice and the operator's responsibility under DR tax law. The proxy ships the tool; the operator decides whether and how to use it, and bears the legal consequences of those decisions.

## Quick start

### 1. Build

```sh
docker build -t mikrowisp-proxy:latest .
```

### 2. Configure

Copy `config.example.toml` to a host directory the container can mount, and edit:

```sh
mkdir -p /etc/mikrowisp-proxy
cp config.example.toml /etc/mikrowisp-proxy/config.toml
$EDITOR /etc/mikrowisp-proxy/config.toml
```

At minimum, set `upstream.base_url` to the real Mikrowisp install. Add `[[filter.exclude]]` blocks for whichever customer field(s) define your exclusion rules.

### 3. Run

```sh
docker run -d --name mw-proxy \
  -p 8080:8080 \
  -v /etc/mikrowisp-proxy:/etc/mikrowisp-proxy:ro \
  --restart=unless-stopped \
  mikrowisp-proxy:latest
```
 
### 4. Point the downstream caller at the proxy

In Axentra (or whatever consumer you're using), set the Mikrowisp `base_url` config to the proxy's address (e.g. `http://your-proxy-host:8080`) instead of the real Mikrowisp URL. The token is unchanged — the proxy never reads or rewrites it.

### 5. Verify

```sh
# Liveness probe
curl http://your-proxy-host:8080/healthz
# {"ok":true}

# Smoke test against the proxy with a real Mikrowisp token
curl -X POST http://your-proxy-host:8080/api/v1/GetInvoices \
  -H 'Content-Type: application/json' \
  -d '{"token":"YOUR_TOKEN","limit":5}'
```

The proxy logs the per-request filter decisions (e.g. `filtered GetInvoices kept=87 dropped=13`) at INFO level.

## Config reference

See `config.example.toml` next to this README — every option is documented there with the rationale for the default.

## Endpoints handled

| Path | Behavior |
|------|----------|
| `/api/v1/GetClientsDetails` | Forwards. On success, drops `datos[]` entries where the customer's extras match any rule. |
| `/api/v1/GetInvoices` | Forwards. On success, drops `facturas[]` entries whose `idcliente` resolves to an excluded customer (verdict cached per customer). |
| `/api/v1/PaidInvoice` | Pass-through. Never modified — writes to upstream are sacred. |
| `/api/v1/Ping` (and any other `/api/...` path) | Pass-through. |
| `/healthz` | Liveness only. Always returns `{"ok":true}`. |

## How the filtering works

1. Inbound request arrives with `{"token":"...","idcliente":...,"limit":...}`.
2. Proxy forwards the request body verbatim to upstream Mikrowisp.
3. Upstream responds. If `estado != "exito"`, the response is forwarded unchanged.
4. For `GetClientsDetails`, the proxy walks each `datos` entry, checks its extras against every rule, and drops it on first match. The verdict is cached per customer id.
5. For `GetInvoices`, the proxy walks each `facturas` entry. For each unique `idcliente`, it consults the verdict cache; on a cache miss it makes a one-shot `GetClientsDetails` call upstream (using the same token from the inbound request) to fetch that customer's extras and decide.
6. The filtered response is re-marshaled and sent back. On no-op filters, the original upstream bytes pass through unchanged.

## Security posture

- **Token handling**: tokens travel through the proxy in memory only. Never logged, never persisted, never observed beyond the request that supplied them.
- **TLS to upstream**: enabled by default. `upstream.insecure_skip_verify` exists for self-signed certs on private LAN — never enable on the public internet.
- **No outbound except upstream**: the proxy makes HTTP calls only to the configured `upstream.base_url`. No telemetry, no analytics.
- **Distroless container**: the Docker image has no shell, no package manager, no extra binaries. Just the proxy and CA certs.
- **Runs as nonroot**: the container's process runs as the unprivileged `nonroot` user.

## Development

```sh
go test ./...        # run the rule-evaluator + cache tests
go build ./...       # build the binary locally
go run . -config ./config.example.toml   # run against the example config (will fail at upstream — set a real base_url first)
```

Project layout:

```
mikrowisp-proxy/
├── main.go                       # entrypoint + signal handling
├── config.example.toml           # the canonical documentation
├── Dockerfile                    # multi-stage, distroless final image
├── internal/
│   ├── config/config.go          # TOML loader + validation
│   ├── mikrowisp/types.go        # endpoint paths + minimal response shapes
│   └── proxy/
│       ├── upstream.go           # HTTP client to the real Mikrowisp
│       ├── filter.go             # rule evaluator + verdict cache
│       └── proxy.go              # HTTP handler dispatching by path
└── README.md
```
