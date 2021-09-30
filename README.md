### sytstemd-shim

This project aims to provide a containerd shim implementation which uses systemd to manage containers.

Advantages over the standard runc (io.containerd.runc.v2) shim:

1. Containers can be seen and managed using systemctl just like any other system service
2. There is a single shim per containerd namespace instead of per container (or pod), so essentially O(1) runtime overhead instead of O(n).
3. Shutting down or restarting the node will correctly shutdown containers because containers are run as systemd units.
4. Possible to send all stdout/stderr messages to journald instead of managing pipes.
5. Shim can be restarted for whatever reason w/o disrupting containers.

Ideally I'd like to make this 1 shim per node, but currently it must be 1 per namespace due to some API issues.
In any case, 1 per namespace is *almost* one per node since in most cases you'll only ever use 1 containerd namespace anyway.

This is alpha quality software and does not yet fully implement the containerd shim API.
Do not use this in production environments.

#### Build:

```shell
$ go build ./cmd/contianerd-shim-systemd-v1
```

#### Usage:

Put the built binary into $PATH (as seen by the containerd daemon).

```shell
# ctr run -t --rm --runtime==io.containerd.systemd.v1 docker.io/busybox:latest test sh
```

You should be able to do things like

```shell
# systemctl status containerd-default-test
```