# syntax=docker/dockerfile:1.24

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.24

# Build Stage — native build per target platform (CGO needs target-arch vips-dev)
FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder

WORKDIR /app

# Install build dependencies (vips-dev for CGO image processing)
RUN apk add --no-cache git wget vips-dev gcc musl-dev

# Download the wait script (architecture-specific)
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
RUN go mod download

# Copy source code
COPY . .

# Build (CGO_ENABLED=1 for libvips, native per-platform build)
RUN CGO_ENABLED=1 go build -tags vips -trimpath -ldflags "-s -w" -o /bin/file-service ./cmd/server/

# Runtime Stage — Alpine for lightweight runtime with vips
FROM alpine:${ALPINE_VERSION}

# Install libvips runtime only
RUN apk add --no-cache vips vips-heif vips-jxl vips-poppler

# Non-root user matching K8s securityContext (fsGroup: 65532)
RUN addgroup -g 65532 -S nonroot && adduser -u 65532 -S -G nonroot nonroot

WORKDIR /app

COPY --from=builder /wait /wait
COPY --from=builder /bin/file-service /bin/file-service

RUN mkdir /storage && chown nonroot:nonroot /storage
VOLUME /storage

USER nonroot:nonroot

EXPOSE 4003

ENTRYPOINT ["/bin/file-service"]
