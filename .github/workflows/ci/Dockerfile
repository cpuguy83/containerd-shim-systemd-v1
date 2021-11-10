# syntax = docker/dockerfile:1.3-labs

ARG GO_VERSION=1.17
FROM golang:${GO_VERSION} AS go

FROM ubuntu:18.04 AS build
RUN apt-get update && apt-get install -y gcc git make btrfs-tools
WORKDIR /go/src/github.com/containerd/containerd
ARG CONTAINERD_REPO=https://github.com/containerd/containerd.git
ARG CONTAINERD_COMMIT=v1.6.0-beta.1
RUN <<EOF
git init .
git remote add origin $CONTAINERD_REPO
git fetch --depth=1 origin ${CONTAINERD_COMMIT}
git checkout FETCH_HEAD
EOF
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ENV PATH=/usr/local/go/bin:$PATH
RUN \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,id=containerd-$TARGETOS$TARGETA$RCH$TARGETVARIANT \
    go mod download
RUN \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,id=containerd-$TARGETOS$TARGETA$RCH$TARGETVARIANT \
    make binaries


FROM scratch
COPY --from=build /go/src/github.com/containerd/containerd/ /