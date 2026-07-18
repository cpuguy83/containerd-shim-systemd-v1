# Agent Guide

## Project context

This repository implements the experimental `io.containerd.systemd.v1`
containerd runtime shim. It is Linux- and systemd-specific, requires
containerd 1.6 or newer, and is still alpha quality with an incomplete shim
API.

Unlike `io.containerd.runc.v2`, which normally has a shim process per
container or pod, this project runs one socket-activated shim service per
node. containerd talks to that service through the runtime-v2 ttrpc task API.
The shim represents containers and exec processes as systemd units, while
`runc` remains the low-level OCI runtime that creates and operates the actual
processes.

## Architecture and runtime flow

- `containerd-shim-systemd-v1 start` reports the shared shim socket to
  containerd; systemd socket activation starts the long-lived `serve`
  process.
- The service implements containerd's task lifecycle operations. Create,
  start, wait, state, kill, exec, and delete are coordinated across in-memory
  process state, systemd units, and `runc`.
- Init processes and exec processes are tracked both by containerd identity
  and by systemd D-Bus object path. Unit names include the containerd
  namespace and task/exec ID.
- A D-Bus event reactor is the source of unit state changes. It reconciles
  exits into task state, wakes waiters, and forwards containerd task events;
  reconnect handling resynchronizes tracked units.
- Rootfs mounting, cgroup/resource handling, logging, and tracing sit around
  that lifecycle. TTY workloads re-exec the binary's small cgo PTY-copy
  helper; non-TTY logging may use stdio or journald.

Keep systemd as the lifecycle authority and `runc` as the OCI process
executor. Changes to task state or event delivery usually need to preserve
both sides of that boundary.

## Build and development environment

The module declares Go 1.23; CI currently builds with a newer Go release.
The binary uses cgo and therefore needs a C compiler as well as a Linux
systemd environment for full testing.

```console
make build                   # writes bin/containerd-shim-systemd-v1
make generate                # refresh generated dependency data
make mod                     # go mod tidy
```

The Nix flake is the easiest reproducible environment:

```console
nix develop
make build
```

The flake builds the shim with gomod2nix for x86_64-linux and aarch64-linux.
After changing `go.mod` or `go.sum`, run `make generate` (or
`go generate ./...`) and include the updated `gomod2nix.toml`.

For isolated end-to-end work, `nix run .#vm` boots an x86_64 NixOS VM with
systemd, containerd, runc, and the shim registered. Validate through `nerdctl`
or `ctr` using runtime `io.containerd.systemd.v1`; Docker requires the patched
dockerd described by the repository Dockerfile.

## Tests

Repository tests share the root Go package; systemd integration tests are not
hidden behind build tags.

```console
go test -short ./...         # unit tests; skips tests that create systemd units
go test ./...                # unit tests plus reachable systemd integration tests
go test -race ./...          # the same suite with the race detector
```

Systemd-dependent tests use the caller's user manager at
`$XDG_RUNTIME_DIR/systemd/private` when available. As root they can use
`/run/systemd/private` and `/run/systemd/system`. They skip in short mode or
when no suitable private bus is reachable; service lifecycle tests also need
a writable unit directory. CI compiles the race-enabled test binary as the
normal user, then runs it as root with `XDG_RUNTIME_DIR` unset so it targets
the system manager.

The broader containerd runtime integration suite uses the repository's
privileged Docker/systemd test image:

```console
make containerd-integration
```

That target requires Docker with buildx and enough privileges for nested
systemd, containerd, mounts, networking, and cgroups. It installs the shim,
runs containerd's integration tests with `TEST_RUNTIME=io.containerd.systemd.v1`,
and writes JUnit output under `bin/test-results/`. `make test-shell` opens the
same test environment for interactive debugging; `make test-daemon` installs
and starts a host test daemon and therefore requires sudo and a running system
systemd.

## Repository conventions

- Most implementation and tests intentionally live in the root `main`
  package; `options/` contains protobuf-generated configuration types and
  `contrib/checkexec/` is a small integration helper.
- Format Go changes with `gofmt`. Follow existing context-aware APIs, wrap
  errors with operation details, and preserve containerd's gRPC error
  translation at API boundaries.
- Protect shared process, unit, event, and wait state consistently. Lifecycle
  changes should cover success, failure, cleanup, and duplicate-event cases.
- Prefer requirement-style test and subtest names. Keep pure unit coverage
  runnable with `-short`, and make live-systemd tests clean up every unit they
  create.
- Do not hand-edit generated protobuf Go files. Update the source definition
  and use the repository generation path.
- Make targets and their variables are part of the development interface;
  reuse them rather than duplicating build or integration setup.
