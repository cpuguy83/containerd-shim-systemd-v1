FROM golang:1.18 AS go

FROM buildpack-deps:bullseye as base
ENV GOROOT=/usr/local/go GOPATH=/go PATH=/go/bin:/usr/local/go/bin:$PATH
COPY --from=go /usr/local/go /usr/local/go

FROM base as build
WORKDIR /go/src/github.com/cpuguy83/containerd-shim-systemd-v1
COPY go.mod .
COPY go.sum .
RUN \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY . .
RUN \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    make

# Only docker's dev branch has support for custom shims right now, so get dockerd from there.
FROM base AS docker
WORKDIR /go/src/github.com/docker/docker
RUN apt-get update && apt-get install -y libbtrfs-dev libdevmapper-dev libltdl-dev
ARG DOCKER_COMMIT=b84225c66b555b98104934b84c38eb9691f2abf3
RUN git init . && git remote add origin https://github.com/cpuguy83/docker.git && git pull origin ${DOCKER_COMMIT}
RUN \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GO111MODULE=off hack/make.sh dynbinary

FROM debian:bullseye as test-img
RUN apt-get update && apt-get install -y systemd curl procps
RUN curl -SLf https://get.docker.com | sh
COPY scripts/docker-entrypoint.sh /usr/local/bin/
COPY --link --from=build /go/src/github.com/cpuguy83/containerd-shim-systemd-v1/bin/* /usr/local/bin/
ENV PATH=/usr/local/bin:${PATH}
COPY --link --from=docker /go/src/github.com/docker/docker/bundles/dynbinary-daemon/dockerd /usr/local/bin/
RUN <<EOF
set -e
mkdir -p /etc/systemd/system/docker.service.d
echo '[Service]' > /etc/systemd/system/docker.service.d/override.conf
echo 'ExecStart=' >> /etc/systemd/system/docker.service.d/override.conf
echo 'ExecStart=/usr/local/bin/dockerd -D -H unix:///var/run/docker.sock -H fd:// --default-runtime=io.containerd.systemd.v1' >> /etc/systemd/system/docker.service.d/override.conf
EOF
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]

FROM scratch
COPY --from=build /go/src/github.com/cpuguy83/containerd-shim-systemd-v1/bin/* /

