package main

import (
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/cpuguy83/systemdshim"
)

func main() {
	shim.Run("io.containerd.systemd.v1", systemdshim.New)
}
