package main

import (
	"bytes"
	"os"
	"testing"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestWriteBootstrapResponse(t *testing.T) {
	const (
		socket    = "/run/containerd/s/systemd.sock"
		id        = "task"
		namespace = "default"
		address   = "unix:///run/containerd/s/systemd.sock"
	)

	t.Run("matching bootstrap parameters return a task v3 protobuf response", func(t *testing.T) {
		var output bytes.Buffer
		input := marshalProto(t, &bootapi.BootstrapParams{
			InstanceID: id,
			Namespace:  namespace,
		})
		if err := writeBootstrapResponse(bytes.NewReader(input), &output, socket, id, namespace); err != nil {
			t.Fatalf("write bootstrap response: %v", err)
		}

		var result bootapi.BootstrapResult
		if err := proto.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatalf("unmarshal bootstrap result: %v", err)
		}
		if result.Version != 3 {
			t.Fatalf("version = %d, want 3", result.Version)
		}
		if result.Address != address {
			t.Fatalf("address = %q, want shared shim socket", result.Address)
		}
		if result.Protocol != "ttrpc" {
			t.Fatalf("protocol = %q, want ttrpc", result.Protocol)
		}
	})

	t.Run("empty input returns the legacy plain-text response", func(t *testing.T) {
		t.Chdir(t.TempDir())
		var output bytes.Buffer
		if err := writeBootstrapResponse(bytes.NewReader(nil), &output, socket, id, namespace); err != nil {
			t.Fatalf("write bootstrap response: %v", err)
		}
		if output.String() != address {
			t.Fatalf("response = %q, want %q", output.String(), address)
		}
		storedAddress, err := os.ReadFile("address")
		if err != nil {
			t.Fatalf("read stored shim address: %v", err)
		}
		if string(storedAddress) != address {
			t.Fatalf("stored address = %q, want %q", storedAddress, address)
		}
	})

	t.Run("legacy runtime options return the legacy plain-text response", func(t *testing.T) {
		t.Chdir(t.TempDir())
		var output bytes.Buffer
		input := marshalProto(t, &anypb.Any{
			TypeUrl: "types.containerd.io/runtime-options",
			Value:   []byte{1, 2, 3},
		})
		if err := writeBootstrapResponse(bytes.NewReader(input), &output, socket, id, namespace); err != nil {
			t.Fatalf("write bootstrap response: %v", err)
		}
		if output.String() != address {
			t.Fatalf("response = %q, want %q", output.String(), address)
		}
	})

	t.Run("mismatched bootstrap identity returns the legacy plain-text response", func(t *testing.T) {
		t.Chdir(t.TempDir())
		var output bytes.Buffer
		input := marshalProto(t, &bootapi.BootstrapParams{
			InstanceID: id,
			Namespace:  "other",
		})
		if err := writeBootstrapResponse(bytes.NewReader(input), &output, socket, id, namespace); err != nil {
			t.Fatalf("write bootstrap response: %v", err)
		}
		if output.String() != address {
			t.Fatalf("response = %q, want %q", output.String(), address)
		}
	})
}

func marshalProto(t *testing.T, message proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("marshal %T: %v", message, err)
	}
	return data
}
