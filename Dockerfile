# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27

FROM golang:${GO_VERSION}-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ocellusai-mcp ./cmd/ocellusai-mcp

# Optional: a kubectl binary for the shell worker. Build with
#   docker build --target with-kubectl --build-arg KUBECTL_VERSION=v1.31.0 .
FROM alpine:3.20 AS kubectl
ARG KUBECTL_VERSION=v1.31.0
ARG TARGETARCH=amd64
RUN apk add --no-cache curl && \
    curl -fsSL -o /kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" && \
    chmod 0755 /kubectl

# Default image: distroless, static, non-root. Only the prometheus worker is
# usable here because there are no CLI binaries inside.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app
COPY --from=build /out/ocellusai-mcp /app/ocellusai-mcp
COPY --chown=65532:65532 tools /app/tools
COPY --chown=65532:65532 config.example.yaml /app/config.yaml
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/ocellusai-mcp"]
CMD ["-config", "/app/config.yaml"]

FROM runtime AS with-kubectl
COPY --from=kubectl /kubectl /usr/local/bin/kubectl
