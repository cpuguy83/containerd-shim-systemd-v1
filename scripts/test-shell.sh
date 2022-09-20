#!/usr/bin/env bash

set -eu -o pipefail

cid="$(cat ${1})"

: ${TEST_SHELL_CMD:=bash}

readonly service="containerd-shim-systemd-v1.service"

set +e

while true; do
    docker exec -it ${cid} /bin/sh -c '[ -f /tmp/init ]' && break
    sleep 1
done

set -e

docker exec -it ${cid} systemctl start containerd-shim-systemd-v1-install.service

docker exec -it ${cid} ${TEST_SHELL_CMD}
docker rm -f ${cid} || true
