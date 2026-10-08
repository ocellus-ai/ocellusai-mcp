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

# Final image: distroless, static, non-root, no CLI binaries inside.
FROM gcr.io/distroless/static-debian12:nonroot AS ocellusai
WORKDIR /app
COPY --from=build /out/ocellusai-mcp /app/ocellusai-mcp
COPY --chown=65532:65532 tools /app/tools
COPY --chown=65532:65532 config.example.yaml /app/config.yaml
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/ocellusai-mcp"]
CMD ["-config", "/app/config.yaml"]

