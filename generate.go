package main

// Regenerate the flake's Go module lock (gomod2nix.toml) from go.mod/go.sum.
// gomod2nix's `generate` computes module hashes in pure Go, so this needs only
// the Go toolchain (a C compiler for this cgo module) and no Nix. Keep the
// pinned version in sync with the flake input.
//go:generate go run github.com/nix-community/gomod2nix@v1.7.0 generate
