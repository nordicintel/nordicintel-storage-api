# syntax=docker/dockerfile:1

# Multi-stage build: the toolchain never reaches the runtime image.
# https://docs.docker.com/build/building/multi-stage/
#
# Both images are pinned by digest so a rebuild of a released commit produces
# the same runtime. Update the digests deliberately, never implicitly.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:4013ae0f9e7994f8535c58c811f8f863fbed38b72e0d51e6592156f758d66146 AS build

WORKDIR /src

# Dependencies change far less often than sources, so resolve them first.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

# CGO_ENABLED=0 produces static binaries that run on a distroless base.
# The "production" tag excludes the test-only down migration, so the shipped
# migrate command can only move the schema forward.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -tags production -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -tags production -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/migrate ./cmd/migrate

# Minimal non-root runtime. The distroless static image ships CA certificates
# and /etc/passwd for the nonroot user, and nothing else: no shell, no package
# manager, and no writable application directory.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/api /app/api
COPY --from=build /out/migrate /app/migrate

# Documented for readers and tooling; the deployment platform still has to
# publish the port and provide the environment.
EXPOSE 8080
USER nonroot:nonroot

# Deployments run /app/migrate as a separate job before rolling out the API.
ENTRYPOINT ["/app/api"]
