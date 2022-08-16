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
ARG DOCKER_COMMIT=724feb898f4af3d7f156fb0064d59a7b68ea9be2
RUN git init . && git remote add origin https://github.com/moby/moby.git && git pull origin ${DOCKER_COMMIT}
RUN \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GO111MODULE=off hack/make.sh dynbinary

FROM debian:bullseye as test-img
RUN apt-get update && apt-get install -y systemd curl
RUN curl -SLf https://get.docker.com | sh
COPY scripts/docker-entrypoint.sh /usr/local/bin/
COPY --link --from=build /go/src/github.com/cpuguy83/containerd-shim-systemd-v1/bin/* /usr/local/bin/
ENV PATH=/usr/local/bin:${PATH}
COPY --link --from=docker /go/src/github.com/docker/docker/bundles/dynbinary-daemon/dockerd /usr/local/bin/
RUN sed -i 's,/usr/bin/dockerd,/usr/local/bin/dockerd,g' /lib/systemd/system/docker.service
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]

FROM scratch
COPY --from=build /go/src/github.com/cpuguy83/containerd-shim-systemd-v1/bin/* /

