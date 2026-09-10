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
        libguestfs-tools \
        qemu-system-x86 \
        linux-image-cloud-amd64 \
    && rm -rf /var/lib/apt/lists/* \
    && command -v virt-customize \
    && ls /boot/vmlinuz* >/dev/null \
    && ls -d /lib/modules/*/ >/dev/null

ENV LIBGUESTFS_BACKEND=direct
ENV LIBGUESTFS_SKIP_OS_CHECK=1

COPY --from=builder /nico-core-mock /nico-core-mock
COPY nico-core-mock/helm/nico-rest-mock-core/values.yaml /config/values.yaml

EXPOSE 11079

USER root
ENTRYPOINT ["/nico-core-mock"]
CMD ["--config", "/config/values.yaml", "--listen", ":11079"]
