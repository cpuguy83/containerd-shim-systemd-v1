package main

import (
	"fmt"
	"os"
	"path/filepath"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"google.golang.org/protobuf/proto"
)

// mountRootfs reads a marshaled CreateTaskRequest from configPath (written by
// initProcess.writeMountConfig) and mounts its rootfs onto <bundle>/rootfs. It
// backs both the `mount` re-exec (ExecStartPre in the no-new-namespace path) and
// the --mounts create path (PrivateMounts).
func mountRootfs(configPath string) error {
	cfgData, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}

	var cfg taskapi.CreateTaskRequest
	if err := proto.Unmarshal(cfgData, &cfg); err != nil {
		return fmt.Errorf("error unmarshalling task create: %w", err)
	}

	if _, err := mountFS(cfg.Rootfs, cfg.Bundle); err != nil {
		return err
	}
	return nil
}

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
