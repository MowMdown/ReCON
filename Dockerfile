# --- Stage 1: Build ---
FROM golang:1.22-alpine AS build

WORKDIR /src

COPY go.mod ./
COPY go.sum* ./

# The program (cmd/recon) and its packages (internal/), including the HTML
# pages embedded by //go:embed in internal/web
COPY cmd ./cmd
COPY internal ./internal

RUN go mod tidy

RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w" -o /recon ./cmd/recon


# --- Stage 2: Runtime ---
FROM alpine:3.20

RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    sqlite

# Create a dedicated non-root user and group (default: 1001:1001)
RUN addgroup -S -g 1001 recon && \
    adduser -S -D -H -u 1001 -G recon recon && \
    mkdir -p /data && \
    chown recon:recon /data

COPY --from=build /recon /usr/local/bin/recon

# Short alias for the command-line tool
# Usage: docker exec <container> rcon players
RUN ln -s /usr/local/bin/recon /usr/local/bin/rcon

WORKDIR /data

USER 1001:1001

EXPOSE 8080

VOLUME ["/data"]

ENTRYPOINT ["recon"]
