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

mount -o remount,rw /proc
mount -o remount,rw /proc/sys

cgroup_mounts="$(mount | grep cgroup | awk '{print $3}')"
for cgroup_mount in ${cgroup_mounts}; do
    umount -l "${cgroup_mount}" || true
done

mount -t cgroup2 cgroup2 /sys/fs/cgroup

exec /lib/systemd/systemd
