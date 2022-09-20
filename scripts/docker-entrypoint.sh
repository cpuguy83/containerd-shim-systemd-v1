#!/bin/bash

set -e

cat /etc/hostname >/tmp/hostname
umount /etc/hostname
mv /tmp/hostname /etc/hostname

cat /etc/hosts >/tmp/hosts
umount /etc/hosts
mv /tmp/hosts /etc/hosts

cat /etc/resolv.conf >/tmp/resolv.conf
umount /etc/resolv.conf
mv /tmp/resolv.conf /etc/resolv.conf

mount -o remount rw /proc
mount -o remount rw /proc/sys

mount -t tmpfs -o uid=0,gid=0,mode=0755 cgroup /sys/fs/cgroup
if [ -e /sys/fs/cgroup/memory/memory.use_hierarchy ]; then
    echo 1 >/sys/fs/cgroup/memory/memory.use_hierarchy
fi

mkdir -p /sys/fs/cgroup/unified
mount -t cgroup2 none /sys/fs/cgroup/unified || true

if [ -n "${TEST_SHIM_CGROUP}" ]; then
    [ -d /sys/fs/cgroup/memory ] &&
        mkdir -p /sys/fs/cgroup/memory/${TEST_SHIM_CGROUP}

    [ -d /sys/fs/cgroup/unified ] &&
        mkdir -p /sys/fs/cgroup/unified/${TEST_SHIM_CGROUP}
fi

echo PID: $$

exec /lib/systemd/systemd
