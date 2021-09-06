package systemdshim

import (
	"context"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"syscall"

	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/pkg/errors"
)

// StartShim is a binary call that executes a new shim returning the address
func (s *service) StartShim(ctx context.Context, opts shim.StartOpts) (_ string, retErr error) {
	address, err := shim.SocketAddress(ctx, opts.Address, grouping)
	if err != nil {
		return "", err
	}

	socket, err := shim.NewSocket(address)
	if err != nil {
		if !shim.SocketEaddrinuse(err) {
			return "", errors.Wrap(err, "create new shim socket")
		}
		if shim.CanConnect(address) {
			if err := shim.WriteAddress("address", address); err != nil {
				return "", errors.Wrap(err, "write existing socket for shim")
			}
			return address, nil
		}
		if err := shim.RemoveSocket(address); err != nil {
			return "", errors.Wrap(err, "remove pre-existing socket")
		}
		if socket, err = shim.NewSocket(address); err != nil {
			return "", errors.Wrap(err, "try create new shim socket 2x")
		}
	}

	defer func() {
		if retErr != nil {
			socket.Close()
			_ = shim.RemoveSocket(address)
		}
	}()

	// make sure that reexec shim-v2 binary use the value if need
	if err := shim.WriteAddress("address", address); err != nil {
		return "", err
	}

	f, err := socket.File()
	if err != nil {
		return "", err
	}

	cmd, err := newCommand(ctx, opts.ContainerdBinary, opts.Address, opts.TTRPCAddress)
	if err != nil {
		return "", err
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)

	if err := cmd.Start(); err != nil {
		f.Close()
		return "", err
	}

	defer func() {
		if retErr != nil {
			cmd.Process.Kill()
		}
	}()
	go cmd.Wait()

	// TODO: options?
	// The runc shim uses this to add the shim process to the specified shim cgroup
	// Since we don't plan to use this, we don't need to do anything, maybe?
	io.Copy(ioutil.Discard, os.Stdin)
	if err := shim.AdjustOOMScore(cmd.Process.Pid); err != nil {
		return "", errors.Wrap(err, "failed to adjust OOM score for shim")
	}
	return address, nil
}

func newCommand(ctx context.Context, bin, containerdAddress, containerdTTRPCAddress string) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-namespace", ns, // shim.Run expects this to be set even though we won't use it (unless we do 1 daemon per ns)
		"-address", containerdAddress,
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=4")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	return cmd, nil
}
