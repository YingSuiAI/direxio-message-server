# Local-only sibling build. The released repository must have no absolute
# capability-api replace. This image adds a BuildKit context replacement inside
# the image layer so a tag that has not been pushed yet can still be built.
# The replacement never changes the source checkout.
# syntax=docker/dockerfile:1.7

FROM docker.io/library/golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY . .
COPY --from=capability_api / /opt/dirextalk-capability-api
RUN go mod edit -replace github.com/YingSuiAI/dirextalk-capability-api=/opt/dirextalk-capability-api
RUN --mount=type=cache,target=/go/pkg/mod go mod download
ARG VERSION=local
ARG REVISION=working-tree
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags netgo,osusergo -ldflags="-s -w -buildid=" -o /out/usr/local/bin/dirextalk-agent ./cmd/dirextalk-agent
RUN install -d -m 0755 /out/etc/ssl/certs /out/etc/dirextalk-agent /out/var/lib/dirextalk-agent \
    && install -d -m 0700 -o 65532 -g 65532 /out/var/lib/dirextalk-agent/extension-staging /out/var/lib/dirextalk-agent/extension-workspaces \
    && install -d -m 1777 /out/tmp \
    && cp /etc/ssl/certs/ca-certificates.crt /out/etc/ssl/certs/ca-certificates.crt

FROM scratch
ARG VERSION=local
ARG REVISION=working-tree
LABEL org.opencontainers.image.title="Dirextalk Agent Core" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION"
COPY --from=build /out/ /
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
USER 65532:65532
WORKDIR /var/lib/dirextalk-agent
EXPOSE 9443 50052
ENTRYPOINT ["/usr/local/bin/dirextalk-agent"]
CMD ["serve"]
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=4 CMD ["/usr/local/bin/dirextalk-agent", "healthcheck"]
