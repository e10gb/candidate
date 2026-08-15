# Built from source inside the image -- no host-compiled binaries copied in.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
# go.sum is generated here rather than committed, so the build stays reproducible
# from just the source in this directory.
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /quoter .

FROM alpine:3.20
COPY --from=build /quoter /usr/local/bin/quoter
ENV NATS_URL=nats://nats:4222
ENTRYPOINT ["quoter"]
