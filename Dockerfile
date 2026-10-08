# --- Stage 1: Build ---
FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY go.sum* ./
COPY *.go ./
RUN go mod tidy

RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w" -o /rcon-bridge .

FROM alpine:3.20

RUN apk add --no-cache \
    ca-certificates \
    tzdata
    
RUN addgroup -g 100 users 2>/dev/null || true && \
    mkdir -p /data && \
    chown -R 99:100 /data

COPY --from=build /rcon-bridge /usr/local/bin/rcon-bridge

RUN ln -s /usr/local/bin/rcon-bridge /usr/local/bin/rcon && \
    ln -s /usr/local/bin/rcon-bridge /rcon-bridge

WORKDIR /data
USER 99:100
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["rcon-bridge"]