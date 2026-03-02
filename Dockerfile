# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/h3ws2h1ws-proxy ./cmd/h3ws2h1ws-proxy

FROM alpine:3.20
WORKDIR /app

RUN apk add --no-cache ca-certificates openssl

COPY --from=builder /out/h3ws2h1ws-proxy /app/h3ws2h1ws-proxy

ENTRYPOINT ["/app/h3ws2h1ws-proxy"]
