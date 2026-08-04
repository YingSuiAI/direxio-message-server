# Local-only sibling build for the optional Core workload-runner profile.
# Runtime hardening and the offline BusyBox shell mirror
# dirextalk-agent/deploy/container/core-runner.Containerfile. The toolchain is
# pinned to the current Go 1.26.5 digest used by the split stack.
# syntax=docker/dockerfile:1.7

FROM docker.io/library/golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
RUN apk add --no-cache busybox-static
COPY go.mod go.sum ./
COPY --from=capability_api / /opt/dirextalk-capability-api
RUN go mod edit -replace github.com/YingSuiAI/dirextalk-capability-api=/opt/dirextalk-capability-api
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags netgo,osusergo -ldflags='-s -w -buildid=' -o /out/usr/local/bin/dirextalk-core-runner ./cmd/dirextalk-core-runner \
    && install -D -m 0555 -o 65530 -g 65530 /bin/busybox.static /out/usr/local/libexec/dirextalk-core-shell \
    && install -d -m 0700 -o 65530 -g 65530 /out/var/lib/dirextalk-core-runner/installs /out/var/lib/dirextalk-core-runner/workspaces /out/var/lib/dirextalk-core-runner/state \
    && install -d -m 0755 /out/cgroup

FROM scratch
ARG VERSION=local
ARG REVISION=working-tree
LABEL org.opencontainers.image.title="Dirextalk Core Workload Runner" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION"
COPY --from=build /out/ /
USER 65530:65530
WORKDIR /var/lib/dirextalk-core-runner
ENTRYPOINT ["/usr/local/bin/dirextalk-core-runner"]
CMD ["serve"]
