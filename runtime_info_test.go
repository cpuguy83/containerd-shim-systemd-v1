package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/api/types"
	v2runcopts "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go/features"
	"google.golang.org/protobuf/proto"
)

func TestWriteRuntimeInfo(t *testing.T) {
	t.Run("a selected runtime reports its OCI features", func(t *testing.T) {
		helperDir := t.TempDir()
		testBinary, err := os.Executable()
		if err != nil {
			t.Fatalf("find test executable: %v", err)
		}
		runcPath := filepath.Join(helperDir, runcStubHelperName)
		if err := os.Symlink(testBinary, runcPath); err != nil {
			t.Fatalf("create runc helper: %v", err)
		}

		runtimeOptions, err := typeurl.MarshalAnyToProto(&v2runcopts.Options{BinaryName: runcPath})
		if err != nil {
			t.Fatalf("marshal runtime options: %v", err)
		}
		optionsData, err := proto.Marshal(runtimeOptions)
		if err != nil {
			t.Fatalf("encode runtime options: %v", err)
		}

		var output bytes.Buffer
		if err := writeRuntimeInfo(context.Background(), bytes.NewReader(optionsData), &output); err != nil {
			t.Fatalf("write runtime info: %v", err)
		}

		var info types.RuntimeInfo
		if err := proto.Unmarshal(output.Bytes(), &info); err != nil {
			t.Fatalf("decode runtime info: %v", err)
		}
		if info.Name != shimName {
			t.Fatalf("runtime name = %q, want %q", info.Name, shimName)
		}
		if !proto.Equal(info.Options, runtimeOptions) {
			t.Fatalf("runtime options = %v, want %v", info.Options, runtimeOptions)
		}

		decoded, err := typeurl.UnmarshalAny(info.Features)
		if err != nil {
			t.Fatalf("decode runtime features: %v", err)
		}
		runtimeFeatures, ok := decoded.(*features.Features)
		if !ok {
			t.Fatalf("runtime features type = %T, want *features.Features", decoded)
		}
		if !contains(runtimeFeatures.MountOptions, "rro") {
			t.Fatalf("runtime mount options = %v, want rro", runtimeFeatures.MountOptions)
		}
	})
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
