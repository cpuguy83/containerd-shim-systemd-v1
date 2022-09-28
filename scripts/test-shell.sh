#!/usr/bin/env bash

set -ux -o pipefail

cid="$(cat ${1})"

trap "docker rm -vf ${cid}" EXIT

: "${TEST_SHELL_CMD:=bash}"
: "${EXTRA_TEST_SHELL_FLAGS:=""}"

readonly service="containerd-shim-systemd-v1.service"

since=0
first=1
while true; do
    if [ "${first}" -eq 1 ]; then
        first=0
        start="$(date +%s)"
    else
        sleep 1
        last=${start}
        start=$(date +%s)
        since=$((start - last))ms
    fi

    docker exec -it ${cid} /bin/sh -c '[ -f /tmp/init ]' && break
    docker logs --since=${since} ${cid}
done

docker exec -it ${cid} systemctl start containerd-shim-systemd-v1-install.service

docker exec -it ${EXTRA_TEST_SHELL_FLAGS} ${cid} ${TEST_SHELL_CMD}
