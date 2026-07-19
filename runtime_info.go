package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"

	"github.com/containerd/containerd/api/types"
	v2runcopts "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go/features"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func writeRuntimeInfo(ctx context.Context, optionsReader io.Reader, output io.Writer) error {
	optionsData, err := io.ReadAll(optionsReader)
	if err != nil {
		return fmt.Errorf("read runtime options: %w", err)
	}

	runcCommand := "runc"
	var runtimeOptions *anypb.Any
	if len(optionsData) > 0 {
		runtimeOptions = &anypb.Any{}
		if err := proto.Unmarshal(optionsData, runtimeOptions); err != nil {
			return fmt.Errorf("unmarshal runtime options: %w", err)
		}
		decoded, err := unmarshalCreateOptions(runtimeOptions)
		if err != nil {
			return fmt.Errorf("decode runtime options: %w", err)
		}
		if opts, ok := decoded.(*v2runcopts.Options); ok && opts.BinaryName != "" {
			runcCommand = opts.BinaryName
		}
	}

	runcPath, err := exec.LookPath(runcCommand)
	if err != nil {
		return fmt.Errorf("look up runc binary %q: %w", runcCommand, err)
	}
	cmd := exec.CommandContext(ctx, runcPath, "features")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	featureData, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("run %s features: %w: %s", runcPath, err, stderr.String())
	}

	var runtimeFeatures features.Features
	if err := json.Unmarshal(featureData, &runtimeFeatures); err != nil {
		return fmt.Errorf("decode %s features: %w", runcPath, err)
	}
	featureOptions, err := typeurl.MarshalAnyToProto(&runtimeFeatures)
	if err != nil {
		return fmt.Errorf("marshal runtime features: %w", err)
	}

	data, err := proto.Marshal(&types.RuntimeInfo{
		Name:     shimName,
		Options:  runtimeOptions,
		Features: featureOptions,
	})
	if err != nil {
		return fmt.Errorf("marshal runtime info: %w", err)
	}
	if _, err := output.Write(data); err != nil {
		return fmt.Errorf("write runtime info: %w", err)
	}
	return nil
}
