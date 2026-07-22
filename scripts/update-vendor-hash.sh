#!/usr/bin/env bash
# Recompute the flake's Go module vendorHash after a go.mod/go.sum change and
# write it into flake.nix. Prints the new hash.
#
# buildGoModule fetches the module set into a fixed-output derivation keyed by
# vendorHash; we deliberately poison it with a placeholder so Nix reports the
# real hash, then patch flake.nix with it.
set -euo pipefail

cd "$(dirname "$0")/.."

fake="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
set_hash() { sed -i -E "s#vendorHash = \"[^\"]*\";#vendorHash = \"$1\";#" flake.nix; }

set_hash "$fake"
got=$(nix build .#containerd-shim-systemd-v1 2>&1 >/dev/null \
        | grep -oE 'sha256-[A-Za-z0-9+/=]{44,}' | tail -n1 || true)

if [ -z "$got" ] || [ "$got" = "$fake" ]; then
	set_hash "$fake" # leave a clearly-invalid marker rather than a half state
	echo "could not determine vendor hash from nix build output" >&2
	exit 1
fi

set_hash "$got"
echo "$got"
