# Built from source inside the image -- no host-compiled binaries copied in.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# Download against the committed go.sum so dependency hashes are verified, rather
# than re-resolving with `go mod tidy` at build time.
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /quoter .

FROM alpine:3.20
COPY --from=build /quoter /usr/local/bin/quoter
ENV NATS_URL=nats://nats:4222
ENTRYPOINT ["quoter"]
