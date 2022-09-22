### sytstemd-shim

This project aims to provide a containerd shim implementation which uses systemd to manage containers.

Advantages over the standard runc (io.containerd.runc.v2) shim:

1. Containers can be seen and managed using systemctl just like any other system service
2. There is a single shim per node instead of per container (or pod), so O(1) runtime overhead instead of O(n).
3. Shutting down or restarting the node will correctly shutdown containers because containers are run as systemd units.
4. Possible to send all stdout/stderr messages to journald instead of managing pipes.
5. Shim can be restarted for whatever reason w/o disrupting containers (TODO).

Because of a bug in containerd 1.5, this currently only works starting from
containerd 1.6.0-beta.1, alternatively you can apply this commit:
https://github.com/containerd/containerd/commit/130a9c7ddbdd33a95a4b531ff25506d2d1651b20
There is a possibility this patch could be backported to containerd 1.5, however
I'd say it is slim since it only helps projects like this one, not current
production workloads.

This is alpha quality software and does not yet fully implement the containerd shim API.
Do not use this in production environments.

Regarding point "2" above, for containers which require a TTY we actually spin up a
helper process to copy from the pty to the stdio pipes. This helper is (mostly)
written in C and has minimal overhead.

#### Build:

```shell
make build
```

#### Install:
```shell
sudo make install # installs binary
$(which containerd-shim-systemd-v1) install # installs/starts systemd units
```

#### Usage:

Put the built binary into $PATH (as seen by the containerd daemon).

```console
# ctr run --rm --runtime=io.containerd.systemd.v1 docker.io/busybox:latest test top
```

You should be able to do things like

```console
# systemctl status containerd-default-test
```