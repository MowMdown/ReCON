# --- Stage 1: Build ---
FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY go.sum* ./
COPY *.go ./
# HTML pages, embedded into the binary by //go:embed in web.go
COPY templates ./templates
RUN go mod tidy

RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w" -o /recon .

FROM alpine:3.20

RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    sqlite

RUN addgroup -g 100 users 2>/dev/null || true && \
    mkdir -p /data && \
    chown -R 99:100 /data

COPY --from=build /recon /usr/local/bin/recon

# `rcon` is a short alias for the command-line tool (docker exec ... rcon players).
RUN ln -s /usr/local/bin/recon /usr/local/bin/rcon

WORKDIR /data
USER 99:100
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["recon"]
