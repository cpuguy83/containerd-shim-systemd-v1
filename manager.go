package systemdshim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	ptypes "github.com/gogo/protobuf/types"
)

// We always use the same grouping so there is a single shim for all containers
const grouping = "systemd-shim"

func New(ctx context.Context, id string, publisher shim.Publisher, shutdown func()) (shim.Shim, error) {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, err
	}
	return &service{conn: conn}, nil
}

type service struct {
	conn *systemd.Conn
}

// Cleanup is a binary call that cleans up any resources used by the shim when the service crashes
func (s *service) Cleanup(ctx context.Context) (*taskapi.DeleteResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

func unitName(ns, id string) string {
	return "containerd-" + ns + "-" + id + ".service"
}

// Create a new container
func (s *service) Create(ctx context.Context, r *taskapi.CreateTaskRequest) (_ *taskapi.CreateTaskResponse, retErr error) {
	ch := make(chan string, 1)
	runc, err := exec.LookPath("runc")
	if err != nil {
		return nil, err
	}
	execStart := []string{runc, "create", "--bundle=" + r.Bundle, r.ID}

	if len(r.Rootfs) > 0 {
		var mounts []mount.Mount
		for _, m := range r.Rootfs {
			mounts = append(mounts, mount.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Options: m.Options,
			})
		}

		rootfs := filepath.Join(r.Bundle, "rootfs")
		if err := os.Mkdir(rootfs, 0711); err != nil && !os.IsExist(err) {
			return nil, err
		}
		if err := mount.All(mounts, rootfs); err != nil {
			return nil, err
		}
		defer func() {
			if retErr != nil {
				mount.UnmountAll(rootfs, 0)
			}
		}()
	}

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	_, err = s.conn.StartTransientUnitContext(ctx, unitName(ns, r.ID), "fail", []systemd.Property{
		systemd.PropExecStart(execStart, true),
		systemd.PropType("forking"),
	}, ch)
	if err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil {
			exec.Command(runc, "delete -f "+r.ID).Run()
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case state := <-ch:
		if state != "done" {
			return nil, fmt.Errorf("error starting init process: %s", state)
		}
	}

	p, err := s.conn.GetServicePropertyContext(ctx, r.ID+".service", "MainPID")
	if err != nil {
		return nil, err
	}
	return &taskapi.CreateTaskResponse{Pid: p.Value.Value().(uint32)}, nil
}

// Start the primary user process inside the container
func (s *service) Start(ctx context.Context, r *taskapi.StartRequest) (*taskapi.StartResponse, error) {
	out, err := exec.Command("runc", "start", r.ID).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("error starting process %q: %w", string(out), err)
	}

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	p, err := s.conn.GetServicePropertyContext(ctx, unitName(ns, r.ID), "MainPID")
	if err != nil {
		return nil, err
	}

	return &taskapi.StartResponse{Pid: p.Value.Value().(uint32)}, nil
}

// Delete a process or container
func (s *service) Delete(ctx context.Context, r *taskapi.DeleteRequest) (*taskapi.DeleteResponse, error) {
	out, err := exec.Command("runc", "delete", "-f", r.ID).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("error deleting %q: %w", string(out), err)
	}
	return &taskapi.DeleteResponse{}, nil
}

// Exec an additional process inside the container
func (s *service) Exec(ctx context.Context, r *taskapi.ExecProcessRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// ResizePty of a process
func (s *service) ResizePty(ctx context.Context, r *taskapi.ResizePtyRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

type runcState struct {
	// Version is the OCI version for the container
	Version string `json:"ociVersion"`
	// ID is the container ID
	ID string `json:"id"`
	// InitProcessPid is the init process id in the parent namespace
	InitProcessPid int `json:"pid"`
	// Status is the current status of the container, running, paused, ...
	Status string `json:"status"`
	// Bundle is the path on the filesystem to the bundle
	Bundle string `json:"bundle"`
	// Rootfs is a path to a directory containing the container's root filesystem.
	Rootfs string `json:"rootfs"`
	// Created is the unix timestamp for the creation time of the container in UTC
	Created time.Time `json:"created"`
	// Annotations is the user defined annotations added to the config.
	Annotations map[string]string `json:"annotations,omitempty"`
	// The owner of the state directory (the owner of the container).
	Owner string `json:"owner"`
}

// State returns runtime state of a process
func (s *service) State(ctx context.Context, r *taskapi.StateRequest) (*taskapi.StateResponse, error) {
	out, err := exec.Command("runc", "state", r.ID).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("error getting state of %q: %w", r.ID, err)
	}

	var st runcState
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, err
	}
	return &taskapi.StateResponse{
		ID:     st.ID,
		Pid:    uint32(st.InitProcessPid),
		Status: toStatus(st.Status),
		Bundle: st.Bundle,
	}, nil
}

func toStatus(s string) task.Status {
	switch s {
	case "created":
		return task.StatusCreated
	case "running":
		return task.StatusRunning
	case "pausing":
		return task.StatusPausing
	case "paused":
		return task.StatusPaused
	case "stopped":
		return task.StatusStopped
	default:
		return task.StatusUnknown
	}
}

// Pause the container
func (s *service) Pause(ctx context.Context, r *taskapi.PauseRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Resume the container
func (s *service) Resume(ctx context.Context, r *taskapi.ResumeRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Kill a process
func (s *service) Kill(ctx context.Context, r *taskapi.KillRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Pids returns all pids inside the container
func (s *service) Pids(ctx context.Context, r *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, r *taskapi.CloseIORequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Checkpoint the container
func (s *service) Checkpoint(ctx context.Context, r *taskapi.CheckpointTaskRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Connect returns shim information of the underlying service
func (s *service) Connect(ctx context.Context, r *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// Shutdown is called after the underlying resources of the shim are cleaned up and the service can be stopped
func (s *service) Shutdown(ctx context.Context, r *taskapi.ShutdownRequest) (*ptypes.Empty, error) {
	os.Exit(0)
	return &ptypes.Empty{}, nil
}

// Stats returns container level system stats for a container and its processes
func (s *service) Stats(ctx context.Context, r *taskapi.StatsRequest) (*taskapi.StatsResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// Update the live container
func (s *service) Update(ctx context.Context, r *taskapi.UpdateTaskRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Wait for a process to exit
func (s *service) Wait(ctx context.Context, r *taskapi.WaitRequest) (*taskapi.WaitResponse, error) {
	return nil, errdefs.ErrNotImplemented
}
