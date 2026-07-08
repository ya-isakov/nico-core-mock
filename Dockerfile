# Build from the parent directory that contains both nico-core-mock and
# infra-controller, for example:
#   docker build -f nico-core-mock/Dockerfile -t nico-core-mock:latest .

FROM --platform=$BUILDPLATFORM golang:1.25.4 AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

ENV CGO_ENABLED=0
ENV GOOS=$TARGETOS
ENV GOARCH=$TARGETARCH

WORKDIR /build

COPY infra-controller/rest-api /build/infra-controller/rest-api
COPY nico-core-mock /build/nico-core-mock

WORKDIR /build/nico-core-mock
RUN go mod download
RUN go build \
    -ldflags "-s -w" \
    -o /nico-core-mock \
    ./cmd/nico-core-mock

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        kmod \
        mount \
        qemu-utils \
        libguestfs-tools \
        qemu-system-x86 \
    && rm -rf /var/lib/apt/lists/*

ENV LIBGUESTFS_BACKEND=direct

COPY --from=builder /nico-core-mock /nico-core-mock
COPY nico-core-mock/helm/nico-rest-mock-core/rendered/machines.yaml /config/machines.yaml

EXPOSE 11079

USER root
ENTRYPOINT ["/nico-core-mock"]
CMD ["--config", "/config/machines.yaml", "--listen", ":11079"]
