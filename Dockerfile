FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /rcon-bridge .

FROM alpine:3.20
COPY --from=build /rcon-bridge /usr/local/bin/rcon-bridge
RUN ln -s /usr/local/bin/rcon-bridge /usr/local/bin/rcon && \
    ln -s /usr/local/bin/rcon-bridge /rcon-bridge
    
EXPOSE 8080
ENTRYPOINT ["rcon-bridge"]