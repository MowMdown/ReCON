# --- Stage 1: Build ---
FROM golang:1.22-alpine AS build

RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /src

COPY go.mod ./
COPY go.sum* ./
RUN go mod download
COPY *.go ./

# Enable CGO so SQLite bindings compile correctly
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o /rcon-bridge .

FROM alpine:3.20

# Install runtime dependencies required for SQLite and SSL
RUN apk add --no-cache sqlite-libs ca-certificates tzdata

# Configure Unraid standard user/group (nobody:users -> 99:100)
RUN getent group 100 || addgroup -g 100 users && \
    id -u 99 >/dev/null 2>&1 || adduser -u 99 -G users -s /bin/false -D nobody && \
    mkdir -p /data && chown -R 99:100 /data

COPY --from=build /rcon-bridge /usr/local/bin/rcon-bridge

RUN ln -s /usr/local/bin/rcon-bridge /usr/local/bin/rcon && \
    ln -s /usr/local/bin/rcon-bridge /rcon-bridge

WORKDIR /data
USER 99:100

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["rcon-bridge"]