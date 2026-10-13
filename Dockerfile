# =============================================================================
# Mikrowisp Proxy — multi-stage Dockerfile
# =============================================================================
# Build stage compiles a static Go binary; final stage is a distroless image
# carrying only the binary + ca-certificates. Result is small (~15MB) and
# has no shell, so the attack surface is just the binary itself.
#
# Build:   docker build -t mikrowisp-proxy:latest .
# Run:     docker run -d --name mw-proxy \
#            -p 8080:8080 \
#            -v /etc/mikrowisp-proxy:/etc/mikrowisp-proxy:ro \
#            mikrowisp-proxy:latest
# =============================================================================

# ----- builder ---------------------------------------------------------------
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache module downloads in their own layer so source-only changes don't
# force a full re-download on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 + a static build -> a binary that can run from scratch.
# -trimpath strips local build paths from the binary; -ldflags="-s -w"
# drops the symbol table + DWARF debug info to shrink the binary.
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -trimpath -ldflags="-s -w" -o /out/mikrowisp-proxy .

# ----- final image -----------------------------------------------------------
# distroless/static is the smallest secure base for a static Go binary.
# It includes a minimal /etc/passwd (with `nonroot` user), CA certs, and
# tzdata; nothing else. No shell, no package manager, no curl.
FROM gcr.io/distroless/static-debian12:nonroot

# Run as the unprivileged `nonroot` user from the base image so that even
# in the very unlikely event of a binary escape, the process can't write
# anywhere meaningful.
USER nonroot:nonroot

WORKDIR /app
COPY --from=builder /out/mikrowisp-proxy /app/mikrowisp-proxy

# Bind port. The value is informational; the actual port is decided by
# the bind setting in config.toml.
EXPOSE 8080

# Default config path. Override with -config flag at run time, or mount
# a config.toml at this exact path.
ENTRYPOINT ["/app/mikrowisp-proxy", "-config", "/etc/mikrowisp-proxy/config.toml"]
