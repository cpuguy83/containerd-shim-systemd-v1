### sytstemd-shim

This project aims to provide a containerd shim implementation which uses systemd to manage containers.

Advantages over the standard runc (io.containerd.runc.v2) shim:

1. Containers can be seen and managed using systemctl just like any other system service
2. There is a single shim per node instead of per container (or pod), so O(1) runtime overhead instead of O(n).
3. Shutting down or restarting the node will correctly shutdown containers because containers are run as systemd units.
4. Possible to send all stdout/stderr messages to journald instead of managing pipes.
5. Shim can be restarted for whatever reason w/o disrupting containers (TODO).

This requires a minimum of containerd 1.6 to function.

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

#### Development environment (Nix):

A flake provides a dev shell for building and iterating, and a throwaway NixOS
VM for isolated end-to-end validation against a real containerd and systemd.

Dev shell (Go toolchain plus containerd, runc, nerdctl, and cni-plugins):

```console
$ nix develop
$ make build
$ go test ./...        # unit + integration tests; needs a systemd user bus
```

VM (boots a clean systemd with containerd and the shim prebuilt and registered
as `io.containerd.systemd.v1`, wired via socket activation):

```console
$ nix run .#vm
# inside the VM (autologin as root):
# nerdctl run --rm --runtime io.containerd.systemd.v1 docker.io/library/busybox:latest echo hi
# systemctl list-units 'io-containerd-systemd-*'
```

The shim's own `install` command writes its units to `/etc/systemd/system`, which
is read-only on NixOS, so the flake's NixOS module declares the equivalent socket
and service units natively (see `flake.nix`). Driving the shim through `docker`
needs a patched dockerd (see `Dockerfile`), so validate with `nerdctl` or `ctr`
against the VM's system containerd.

The flake builds the shim with [gomod2nix](https://github.com/nix-community/gomod2nix):
the Go module set is pinned in `gomod2nix.toml`. Regenerate it after a
`go.mod`/`go.sum` change with `make generate` (or `go generate ./...`), which runs
gomod2nix's pure-Go generator — no Nix required — and commit the result.
