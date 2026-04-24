# Stage 1 - build the bridge binary
FROM golang:1.25.1 AS builder

ARG BUILD_COMMIT=unknown
ARG BUILD_DATE=unknown
ARG BUILD_VERSION=v1.0-beta2

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN set -euo pipefail; \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -ldflags="-s -w -X main.buildCommit=${BUILD_COMMIT} -X main.buildVersion=${BUILD_VERSION} -X main.buildDate=${BUILD_DATE}" \
      -o /out/xworkmate-bridge .

# Stage 2 - minimal runtime image
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/xworkmate-bridge /usr/local/bin/xworkmate-bridge

EXPOSE 8787

ENTRYPOINT ["/usr/local/bin/xworkmate-bridge", "serve", "--listen", "0.0.0.0:8787"]
