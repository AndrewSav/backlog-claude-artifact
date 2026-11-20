# Build from the repo root (docker build -f server/Dockerfile .): the server
# embeds plugin/shared/list-controls.js through assets.go.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY assets.go ./
COPY plugin/shared/list-controls.js plugin/shared/
COPY server/*.go server/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backlog-server ./server

# Starts as root only to take ownership of /data, then drops to uid 65532
# (see privdrop_linux.go).
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/backlog-server /backlog-server
EXPOSE 8080
ENTRYPOINT ["/backlog-server"]
