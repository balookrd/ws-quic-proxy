FROM golang:1.25.1-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/ws-quic-proxy ./cmd/ws-quic-proxy

FROM gcr.io/distroless/static-debian12

WORKDIR /app

ENV GODEBUG="http2xconnect=1"

COPY --from=build /out/ws-quic-proxy /app/ws-quic-proxy

ENTRYPOINT ["/app/ws-quic-proxy"]
