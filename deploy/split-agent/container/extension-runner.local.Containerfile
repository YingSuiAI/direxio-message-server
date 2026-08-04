# Local-only sibling build for the optional extension profile. This is kept in
# the message-server harness so both projects can be built from one Compose
# command; the hardened runtime shape mirrors dirextalk-agent/deploy/container.
# The released image is built from the same Agent source revision and pinned
# separately before publication.
# syntax=docker/dockerfile:1.7

FROM docker.io/library/golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
COPY . .
COPY --from=capability_api / /opt/dirextalk-capability-api
RUN go mod edit -replace github.com/YingSuiAI/dirextalk-capability-api=/opt/dirextalk-capability-api
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags netgo,osusergo -ldflags='-s -w -buildid=' -o /out/usr/local/bin/dirextalk-extension-runner ./cmd/dirextalk-extension-runner
RUN install -d -m 0770 -o 65531 -g 65532 /out/run/dirextalk-agent \
    && install -d -m 0700 -o 65531 -g 65531 /out/var/lib/dirextalk-agent/extension-install \
      /out/var/lib/dirextalk-agent/extension-workspaces /out/var/lib/dirextalk-agent/extension-state \
    && install -d -m 0755 /out/cgroup

FROM scratch
ARG VERSION=local
ARG REVISION=working-tree
LABEL org.opencontainers.image.title="Dirextalk Extension Runner" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION"
COPY --from=build /out/ /
USER 65531:65531
ENTRYPOINT ["/usr/local/bin/dirextalk-extension-runner"]
CMD ["serve"]
