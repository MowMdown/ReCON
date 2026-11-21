# --- Stage 1: Build ---
FROM golang:1.22-alpine AS build

WORKDIR /src

COPY go.mod ./
COPY go.sum* ./

# The program (cmd/recon) and its packages (internal/), including the HTML
# pages embedded by //go:embed in internal/web and internal/livemap
COPY cmd ./cmd
COPY internal ./internal

RUN go mod tidy

# The release tag (the image workflow passes it), shown at the bottom right
# of every page.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w -X github.com/MowMdown/ReCON/internal/web.Version=${VERSION}" -o /recon ./cmd/recon


# --- Stage 1b: the recon-mod this ReCON needs ---
# recon-mod.version names a ReCON-Mod release; its ReCON-Mod.zip (the
# @ReCON folder) goes into the image, with the tag in a hidden .version.
# On start ReCON copies it to /data/@ReCON when that's another version.
# A release that doesn't exist fails the build.
FROM alpine:3.20 AS mod
WORKDIR /out
COPY recon-mod.version /recon-mod.version
RUN tag="$(tr -d ' \r\n' < /recon-mod.version)" && \
    wget -q -O /tmp/mod.zip "https://github.com/MowMdown/ReCON-Mod/releases/download/${tag}/ReCON-Mod.zip" && \
    unzip -q /tmp/mod.zip -d /out && \
    test -f /out/@ReCON/addons/ReCON.pbo && \
    echo "$tag" > /out/@ReCON/.version


# --- Stage 2: Runtime ---
FROM alpine:3.20

# The container's icon in Unraid's Docker tab (and other dashboards that read
# the label). It's a URL, not a file in the image: the app icon in this repo.
LABEL org.opencontainers.image.title="ReCON" \
      org.opencontainers.image.source="https://github.com/MowMdown/ReCON" \
      net.unraid.docker.icon="https://raw.githubusercontent.com/MowMdown/ReCON/main/internal/web/assets/icons/dayz-rcon-app-icon-256.png"

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

# The recon-mod to hand out (see Stage 1b).
COPY --from=mod /out/@ReCON /opt/recon/@ReCON

# Short alias for the command-line tool
# Usage: docker exec <container> rcon players
RUN ln -s /usr/local/bin/recon /usr/local/bin/rcon

WORKDIR /data

USER 1001:1001

EXPOSE 8080

VOLUME ["/data"]

ENTRYPOINT ["recon"]
