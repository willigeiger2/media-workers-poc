# =============================================================================
# Stage 0: Go builder
# =============================================================================
FROM golang:1.25-alpine AS go-builder

# Copy Cloudflare root CA from build context and install it.
# This is needed because Cloudflare WARP intercepts HTTPS and uses its own CA.
# The cert must be installed BEFORE running apk.
COPY container/CloudflareRootCertificateCombined.pem /usr/local/share/ca-certificates/Cloudflare_CA.crt
RUN cat /usr/local/share/ca-certificates/Cloudflare_CA.crt >> /etc/ssl/certs/ca-certificates.crt

WORKDIR /app

# Install git and ca-certificates for Go module downloads.
RUN apk add --no-cache git ca-certificates

COPY container/go.mod container/go.sum ./
RUN go mod download

COPY container/main.go container/handler.go ./
COPY container/ffmpeg/ ./ffmpeg/
COPY container/media/ ./media/
COPY container/overlay/ ./overlay/

RUN go build -trimpath -o /media-poc ./main.go ./handler.go

# =============================================================================
# Stage 1: Runtime
# =============================================================================
FROM alpine:3.21

# Copy Cloudflare root CA from build context and install it.
COPY container/CloudflareRootCertificateCombined.pem /usr/local/share/ca-certificates/Cloudflare_CA.crt
RUN cat /usr/local/share/ca-certificates/Cloudflare_CA.crt >> /etc/ssl/certs/ca-certificates.crt

RUN apk add --no-cache ca-certificates ffmpeg font-liberation font-dejavu

# Copy overlay assets into the image.
COPY cf-logo.png /app/overlays/cf-logo.png

COPY --from=go-builder /media-poc /usr/local/bin/media-poc

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/media-poc"]
