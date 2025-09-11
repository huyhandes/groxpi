# Multi-stage Dockerfile for groxpi
# Supports both amd64 and arm64 architectures
# Stage 1: Build stage
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

# Build arguments for cross-compilation
ARG TARGETOS
ARG TARGETARCH

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

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
    -ldflags='-w -s -extldflags "-static"' \
    -a -installsuffix cgo \
    -o groxpi \
    cmd/groxpi/main.go

# Stage 2: Runtime stage
FROM scratch

# Copy timezone data and certificates from builder
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary
COPY --from=builder /build/groxpi /groxpi

# Copy templates
COPY --from=builder /build/templates /templates

# Create cache directory
VOLUME ["/cache"]

# Set environment variables
ENV GROXPI_CACHE_DIR=/cache
ENV GROXPI_INDEX_URL=https://pypi.org/simple/
ENV GROXPI_CACHE_SIZE=5368709120
ENV GROXPI_INDEX_TTL=1800
ENV PORT=5000

# Expose port
EXPOSE 5000

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/groxpi", "--health-check"] || exit 1

# Run the application
ENTRYPOINT ["/groxpi"]
