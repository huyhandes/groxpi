# Multi-stage Dockerfile for groxpi
# Supports both amd64 and arm64 architectures
# Stage 1: Build stage
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# Build arguments for cross-compilation
ARG TARGETOS
ARG TARGETARCH

# Install build dependencies
RUN apk add --no-cache ca-certificates

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application with optimizations for target architecture
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags='-w -s' \
    -o groxpi \
    ./cmd/groxpi

# Stage 2: Runtime stage
FROM scratch

# Copy CA certificates from builder (nothing loads a time zone, so no tzdata)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary
COPY --from=builder /build/groxpi /groxpi

# No templates are copied: the administrative interface and its interaction
# library are embedded in the binary with go:embed, so the image needs nothing
# but the binary.

# Create cache directory
VOLUME ["/cache"]

# The one setting whose default differs from config.go: the cache lives on the volume.
ENV GROXPI_CACHE_DIR=/cache

# Expose port
EXPOSE 5000

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/groxpi", "--health-check"]

# Run the application
ENTRYPOINT ["/groxpi"]
