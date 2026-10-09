# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# BuildKit sets TARGETOS and TARGETARCH for each platform of a
# multi-platform build (docker buildx build --platform linux/amd64,linux/arm64).
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/saige ./cmd/saige-mcp

# ---- Runtime stage ----
FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /out/saige /out/saige-mcp /usr/local/bin/

ENTRYPOINT ["saige"]
