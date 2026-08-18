# Stage 1: Build dockflare binary
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /dockflare ./cmd/dockflare

# Stage 2: Download cloudflared
FROM alpine:3.19 AS cloudflared-dl
RUN apk add --no-cache curl
# Pin to a specific version in production; see https://github.com/cloudflare/cloudflared/releases
ARG CLOUDFLARED_VERSION=2024.6.1
RUN curl -fsSL -o /cloudflared \
    "https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}/cloudflared-linux-amd64" \
    && chmod +x /cloudflared

# Stage 3: Final minimal image
FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /dockflare /usr/local/bin/dockflare
COPY --from=cloudflared-dl /cloudflared /usr/local/bin/cloudflared
VOLUME ["/config"]
ENTRYPOINT ["dockflare", "--config", "/config/config.yml"]
