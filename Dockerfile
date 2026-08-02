# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git ca-certificates

# Copy go mod files and vendor directory first for better layer caching
COPY go.mod go.sum ./
COPY vendor/ ./vendor/

# Copy source code
COPY . .

# Build the binary using vendor dependencies
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -ldflags="-w -s" -o /futureq ./cmd/futureq

# Runtime stage
FROM alpine:3.24

WORKDIR /app

# Install ca-certificates for HTTPS and runas non-root user
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 appuser

USER appuser

# Copy binary from builder
COPY --from=builder /futureq /app/futureq

# Expose default gRPC port (can be overridden via config)
EXPOSE 50051

ENTRYPOINT ["/app/futureq"]
CMD ["start"]
