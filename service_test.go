package main

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"

	taskv2 "github.com/containerd/containerd/api/runtime/task/v2"
	taskv3 "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/ttrpc"
)

func TestServiceRegistersTaskAPIVersions(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "shim.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on shim socket: %v", err)
	}

	server, err := newService(&Service{})
	if err != nil {
		t.Fatalf("create shim service: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(ctx, listener)
	}()

	connection, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("connect to shim service: %v", err)
	}
	client := ttrpc.NewClient(connection)

	t.Cleanup(func() {
		client.Close()
		cancel()
		server.Close()
		listener.Close()
		if err := <-serverErr; err != nil && !errors.Is(err, ttrpc.ErrServerClosed) {
			t.Errorf("serve shim API: %v", err)
		}
	})

	t.Run("a task v2 client can call the shared service", func(t *testing.T) {
		taskClient := taskv2.NewTTRPCTaskClient(client)
		if _, err := taskClient.Shutdown(ctx, &taskv2.ShutdownRequest{ID: "task"}); err != nil {
			t.Fatalf("call task v2 service: %v", err)
		}
	})

	t.Run("a task v3 client can call the shared service", func(t *testing.T) {
		taskClient := taskv3.NewTTRPCTaskClient(client)
		if _, err := taskClient.Shutdown(ctx, &taskv3.ShutdownRequest{ID: "task"}); err != nil {
			t.Fatalf("call task v3 service: %v", err)
		}
	})
}
