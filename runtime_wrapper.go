package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"

	"github.com/containerd/go-runc"
	"golang.org/x/sys/unix"
)

const runcWrapperFlag = "--containerd-shim-systemd-runc-wrapper"

func (p *process) runcForBundle() *runc.Runc {
	runtime := *p.runc
	command := runtime.Command
	if command == "" {
		command = runc.DefaultCommand
	}
	runtime.Command = p.exe
	runtime.ExtraArgs = append(
		slices.Clone(runtime.ExtraArgs),
		runcWrapperFlag, command, p.root,
	)
	return &runtime
}

func runRuncWrapper(args []string) (bool, error) {
	for i, arg := range args {
		if arg != runcWrapperFlag {
			continue
		}
		if len(args) < i+3 {
			return true, fmt.Errorf("%s requires a runtime and bundle", runcWrapperFlag)
		}

		command, err := exec.LookPath(args[i+1])
		if err != nil {
			return true, fmt.Errorf("look up OCI runtime %q: %w", args[i+1], err)
		}
		if err := os.Chdir(args[i+2]); err != nil {
			return true, fmt.Errorf("change to bundle %q: %w", args[i+2], err)
		}

		runtimeArgs := make([]string, 0, len(args)-3)
		runtimeArgs = append(runtimeArgs, args[:i]...)
		runtimeArgs = append(runtimeArgs, args[i+3:]...)
		return true, unix.Exec(command, append([]string{command}, runtimeArgs...), os.Environ())
	}
	return false, nil
}
