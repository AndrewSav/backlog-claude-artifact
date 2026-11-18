FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backlog-server .

# Starts as root only to take ownership of /data, then drops to uid 65532
# (see privdrop_linux.go).
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/backlog-server /backlog-server
EXPOSE 8080
ENTRYPOINT ["/backlog-server"]
