# Build Stage
FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /cosmos .

# Runtime Stage
FROM alpine:3.19

RUN apk add --no-cache ca-certificates curl bash msmtp
# Install MinIO Client (mc)
RUN curl -sSL https://dl.min.io/client/mc/release/linux-amd64/mc -o /usr/local/bin/mc && \
    chmod +x /usr/local/bin/mc

WORKDIR /app
COPY --from=builder /cosmos /app/cosmos
COPY templates /app/templates

EXPOSE 8080

ENTRYPOINT ["/app/cosmos"]
