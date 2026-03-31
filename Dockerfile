# Build Stage — runs on target platform (not cross-compiled)
FROM golang:1.25-bookworm AS builder

WORKDIR /app

# Install build dependencies (libvips-dev for CGO image processing)
RUN apt-get update && apt-get install -y --no-install-recommends \
    libvips-dev \
    wget \
    && rm -rf /var/lib/apt/lists/*

# Download the wait script
ARG TARGETARCH
RUN if [ "$TARGETARCH" = "arm64" ]; then \
      wget -O /wait https://github.com/ufoscout/docker-compose-wait/releases/download/2.12.1/wait_aarch64; \
    elif [ "$TARGETARCH" = "amd64" ]; then \
      wget -O /wait https://github.com/ufoscout/docker-compose-wait/releases/download/2.12.1/wait; \
    else \
      echo "Unsupported architecture: $TARGETARCH" && exit 1; \
    fi && chmod +x /wait

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application (CGO_ENABLED=1 required for libvips)
RUN CGO_ENABLED=1 go build -tags vips -o /bin/file-service ./cmd/server/

# Runtime Stage — same platform as builder, no QEMU issues
FROM debian:bookworm-slim

# Install libvips runtime only (no dev headers, no CA certs — TLS terminates at Traefik)
RUN apt-get update && apt-get install -y --no-install-recommends \
    libvips42 \
    && rm -rf /var/lib/apt/lists/*

# Non-root user matching existing K8s securityContext (fsGroup: 65532)
RUN groupadd -g 65532 nonroot && useradd -u 65532 -g nonroot -s /bin/false nonroot

WORKDIR /app

# Copy wait script from builder
COPY --from=builder /wait /wait
# Copy binary from builder
COPY --from=builder /bin/file-service /bin/file-service

USER nonroot:nonroot

EXPOSE 4003

ENTRYPOINT ["/bin/file-service"]
