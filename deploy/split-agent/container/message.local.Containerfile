# Local-only source build. Dependencies resolve from the committed public Go
# module graph; no sibling source or go.mod replacement enters this build.
# syntax=docker/dockerfile:1.7

FROM docker.io/library/golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
RUN apk --update --no-cache add bash build-base git
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=1 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w" \
    -o /out/ \
      ./cmd/dirextalk-message-server \
      ./cmd/generate-config \
      ./cmd/generate-keys \
      ./cmd/create-account

FROM docker.io/library/alpine:latest@sha256:55ae5d250caebc548793f321534bc6a8ef1d116f334f18f4ada1b2daad3251b2
RUN apk --update --no-cache add bash ca-certificates openssl
ARG VERSION=local
ARG COMMIT=working-tree
ARG BUILD_TIME=
LABEL org.opencontainers.image.title="Dirextalk Message Server" \
      org.opencontainers.image.description="Dirextalk Matrix homeserver and P2P product API server" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT" \
      org.opencontainers.image.created="$BUILD_TIME"
COPY --from=build /out/generate-config /usr/bin/generate-config
COPY --from=build /out/generate-keys /usr/bin/generate-keys
COPY --from=build /out/create-account /usr/bin/create-account
COPY --from=build /out/dirextalk-message-server /usr/bin/dirextalk-message-server
VOLUME /etc/dirextalk-message-server
WORKDIR /etc/dirextalk-message-server
ENTRYPOINT ["/usr/bin/dirextalk-message-server"]
EXPOSE 8008 8448 50053
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=30 CMD wget -q -O - http://127.0.0.1:8008/_p2p/health >/dev/null && wget --no-check-certificate -q -O - https://127.0.0.1:8448/_p2p/health >/dev/null || exit 1
