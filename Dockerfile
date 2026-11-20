# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS builder

WORKDIR /app

# Download dependencies separately so source changes reuse this layer.
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -mod=readonly -trimpath -ldflags="-w -s" -o /futureq ./cmd/futureq

# Runtime stage
FROM alpine:3.24

WORKDIR /app

# Default storage paths must be writable by the runtime user.
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 appuser && \
    mkdir -p /app/data /app/raft-data && \
    chown -R appuser:appuser /app

USER appuser

# Copy binary from builder
COPY --from=builder /futureq /app/futureq

# gRPC, Prometheus metrics, and Raft transport.
EXPOSE 8443 9090 50005

ENTRYPOINT ["/app/futureq"]
CMD ["start"]
