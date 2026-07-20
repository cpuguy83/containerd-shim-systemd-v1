package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/go-runc"
)

func TestRuntimeInvocationsUseBundleWorkingDirectory(t *testing.T) {
	t.Run("a runtime state call reads bundle-relative configuration", func(t *testing.T) {
		bundle := t.TempDir()
		if err := os.WriteFile(filepath.Join(bundle, "config.json"), []byte("{}"), 0600); err != nil {
			t.Fatalf("write bundle config: %v", err)
		}

		p := &process{
			id:   "container",
			root: bundle,
			exe:  testExecutable(t),
			runc: &runc.Runc{
				Command: newRuncStub(t),
				Root:    t.TempDir(),
			},
		}

		state, err := p.runcForBundle().State(context.Background(), p.id)
		if err != nil {
			t.Fatalf("read runtime state: %v", err)
		}
		if state.Bundle != bundle {
			t.Fatalf("runtime bundle = %q, want %q", state.Bundle, bundle)
		}
	})
}

func TestInitPIDFile(t *testing.T) {
	t.Run("the init PID uses the conventional bundle path", func(t *testing.T) {
		bundle := t.TempDir()
		p := &initProcess{
			process: &process{root: bundle, id: "container"},
			Bundle:  bundle,
		}

		want := filepath.Join(bundle, "init.pid")
		if got := p.pidFile(); got != want {
			t.Fatalf("init PID file = %q, want %q", got, want)
		}
	})
}
