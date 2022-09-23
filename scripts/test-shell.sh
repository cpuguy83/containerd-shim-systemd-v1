#!/usr/bin/env bash

set -eux -o pipefail

env | grep GOTEST

cid="$(cat ${1})"

: "${TEST_SHELL_CMD:=bash}"
: "${EXTRA_TEST_SHELL_FLAGS:=""}"

readonly service="containerd-shim-systemd-v1.service"

set +e

while true; do
    docker exec -it ${cid} /bin/sh -c '[ -f /tmp/init ]' && break
    docker logs ${cid}
    sleep 1
done

set -e

docker exec -it ${cid} systemctl start containerd-shim-systemd-v1-install.service

docker exec -it ${EXTRA_TEST_SHELL_FLAGS} ${cid} ${TEST_SHELL_CMD}
docker rm -f ${cid} || true
