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

cat <<EOF >/lib/systemd/system/containerd-shim-systemd-v1-install.service
[Unit]
Description=Install systemd shim
After=local-fs.target

[Service]
ExecStart=/usr/local/bin/containerd-shim-systemd-v1 install --debug --log-mode=journald
RemainAfterExit=true
Type=oneshot

[Install]
WantedBy=multi-user.target
EOF

systemctl enable containerd-shim-systemd-v1-install.service

exec /lib/systemd/systemd
