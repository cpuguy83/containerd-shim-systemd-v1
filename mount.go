package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/mount"
)

func mountFS(tmounts []*types.Mount, bundle string) (string, error) {
	var mounts []mount.Mount
	for _, m := range tmounts {
		mounts = append(mounts, mount.Mount{
			Type:    m.Type,
			Source:  m.Source,
			Options: m.Options,
		})
	}

	rootfs := filepath.Join(bundle, "rootfs")
	if err := os.Mkdir(rootfs, 0700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("error creating rootfs dir: %w", err)
	}
	return rootfs, mount.All(mounts, rootfs)
}
