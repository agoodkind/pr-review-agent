ARG GO_VERSION
FROM golang:${GO_VERSION}-bookworm AS go
FROM docker:28-cli AS docker
FROM node:24-bookworm
COPY --from=go /usr/local/go /usr/local/go
COPY --from=docker /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
ENV PATH="/usr/local/go/bin:${PATH}"
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY config ./config
COPY runtime.json ./runtime.json
COPY deploy/cloudflare ./deploy/cloudflare
WORKDIR /workspace/deploy/cloudflare
RUN npm ci
