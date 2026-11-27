# ==================== BUILD STAGE ====================
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /app/server ./cmd/api/

# ==================== PRODUCTION STAGE ====================
FROM alpine:3.19

RUN addgroup -S app && adduser -S app -G app && \
    apk add --no-cache ca-certificates tzdata curl

WORKDIR /app
COPY --from=builder --chown=app:app /app/server .

USER app

# Health check for Railway/Docker
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD curl -f http://localhost:${PORT:-8080}/live || exit 1

EXPOSE 8080

CMD ["./server"]
