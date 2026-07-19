package main

import (
	"testing"

	"github.com/coreos/go-systemd/v22/dbus"
)

func TestTTYServiceProperties(t *testing.T) {
	tests := []struct {
		name   string
		stdin  string
		stdout string
	}{
		{
			name:   "empty stream paths do not set systemd file properties",
			stdin:  "",
			stdout: "",
		},
		{
			name:   "configured stream paths set systemd file properties",
			stdin:  "/tmp/stdin",
			stdout: "/tmp/stdout",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &process{
				exe:    "/usr/bin/shim",
				Stdin:  tc.stdin,
				Stdout: tc.stdout,
			}
			properties := p.ttyServiceProperties("/tmp/tty.sock", "/tmp/tty.log")

			assertTTYFileProperty(t, properties, "StandardInputFile", tc.stdin)
			assertTTYFileProperty(t, properties, "StandardOutputFile", tc.stdout)
		})
	}
}

func assertTTYFileProperty(t *testing.T, properties []dbus.Property, name, want string) {
	t.Helper()

	for _, property := range properties {
		if property.Name != name {
			continue
		}
		got, ok := property.Value.Value().(string)
		if !ok {
			t.Fatalf("%s value type = %T, want string", name, property.Value.Value())
		}
		if want == "" {
			t.Fatalf("%s is set to %q, want it omitted", name, got)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
		return
	}
	if want != "" {
		t.Fatalf("%s is omitted, want %q", name, want)
	}
}
