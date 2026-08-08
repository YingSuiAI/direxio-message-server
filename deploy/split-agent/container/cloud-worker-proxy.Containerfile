ARG ALPINE_BASE=docker.io/library/alpine:latest@sha256:55ae5d250caebc548793f321534bc6a8ef1d116f334f18f4ada1b2daad3251b2
FROM ${ALPINE_BASE}

RUN apk add --no-cache ca-certificates squid \
    && squid -v \
    && rm -f /etc/squid/squid.conf

ENTRYPOINT ["squid"]
