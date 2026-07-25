package main

import (
	"strings"
	"testing"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/go-runc"
	systemd "github.com/coreos/go-systemd/v22/dbus"
)

// A non-empty Rootfs selects one of two mount strategies, both expressed as
// transient unit properties. This proves each builds the right properties
// without a live bus: the no-new-namespace path uses ExecStartPre/ExecStopPost
// re-execs, and the default path uses PrivateMounts plus a --mounts create. The
// no-new-namespace runtime is additionally exercised end to end by
// TestServiceTaskRootfsMountAgainstSystemd.
func TestInitProcessRootfsMountProperties(t *testing.T) {
	newProc := func(noNewNamespace bool) *initProcess {
		p, _ := newTestInitProcess("mount-props")
		p.Bundle = "/bundle"
		p.root = "/root"
		p.exe = "/shim"
		p.runc = &runc.Runc{Command: "/runc", Root: "/runc/root"}
		p.Rootfs = []*types.Mount{{Type: "bind", Source: "/src", Options: []string{"rbind"}}}
		p.noNewNamespace = noNewNamespace
		return p
	}

	t.Run("the no-new-namespace path mounts via ExecStartPre and unmounts via ExecStopPost", func(t *testing.T) {
		props, err := newProc(true).startProperties([]string{"create"})
		if err != nil {
			t.Fatalf("startProperties: %v", err)
		}
		if has(props, "PrivateMounts") {
			t.Fatal("no-new-namespace unit should not set PrivateMounts")
		}
		if pre := strings.Join(execArgs(t, props, "ExecStartPre"), " "); !strings.Contains(pre, "mount") {
			t.Fatalf("ExecStartPre = %q, want a mount command", pre)
		}
		if post := strings.Join(execArgs(t, props, "ExecStopPost"), " "); !strings.Contains(post, "unmount") {
			t.Fatalf("ExecStopPost = %q, want an unmount command", post)
		}
	})

	t.Run("the default path mounts via PrivateMounts and a --mounts create", func(t *testing.T) {
		props, err := newProc(false).startProperties([]string{"create"})
		if err != nil {
			t.Fatalf("startProperties: %v", err)
		}
		if !has(props, "PrivateMounts") {
			t.Fatal("default unit should set PrivateMounts")
		}
		if has(props, "ExecStartPre") {
			t.Fatal("default unit should not set ExecStartPre")
		}
		if start := strings.Join(execArgs(t, props, "ExecStart"), " "); !strings.Contains(start, "--mounts=") {
			t.Fatalf("ExecStart = %q, want a --mounts create", start)
		}
	})
}

func has(props []systemd.Property, name string) bool {
	for _, p := range props {
		if p.Name == name {
			return true
		}
	}
	return false
}

// execArgs returns the argv of the named Exec* transient property.
func execArgs(t *testing.T, props []systemd.Property, name string) []string {
	t.Helper()
	for _, p := range props {
		if p.Name != name {
			continue
		}
		cmds, ok := p.Value.Value().([]execCommand)
		if !ok || len(cmds) == 0 {
			t.Fatalf("%s value = %T, want a non-empty []execCommand", name, p.Value.Value())
		}
		return cmds[0].Args
	}
	t.Fatalf("no %s property found", name)
	return nil
}
