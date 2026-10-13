# Koro single image (docs/design/05 §1.4): multi-stage, static binary,
# distroless, non-root. Base images are pinned by version + digest ("latest"
# is forbidden); comments stay on their own lines.
# Build: docker build -f deploy/Dockerfile --build-arg GIT_SHA=$(git rev-parse --short HEAD) -t koro:<sha> .
FROM golang:1.27.2@sha256:007a316059246276cb214df93950be77837545dc78007a73e0291a4963d5d3bf AS build
ARG GIT_SHA=dev
ENV GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly \
    CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w -X main.version=${GIT_SHA}" -o /out/koro ./cmd/koro

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/koro /koro
USER nonroot:nonroot
EXPOSE 8080
# Probes the API's /healthz on HTTP_ADDR; worker/migrate services override or
# disable it in Dokploy (deploy/README.md).
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 CMD ["/koro", "healthcheck"]
# No CMD: the Dokploy service gives the command: "api" | "worker" | "migrate up".
ENTRYPOINT ["/koro"]
