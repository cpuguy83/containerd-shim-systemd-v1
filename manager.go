package systemdshim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/go-runc"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	ptypes "github.com/gogo/protobuf/types"
)

// We always use the same grouping so there is a single shim for all containers
const grouping = "systemd-shim"

func New(ctx context.Context, id string, publisher shim.Publisher, shutdown func()) (shim.Shim, error) {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	return &service{
		conn: conn,
		root: filepath.Dir(filepath.Dir(cwd)), // right now, shim.Run sticks us in a dir for the task ID that's being run
		runc: &runc.Runc{
			SystemdCgroup: true,
			PdeathSignal:  syscall.SIGKILL,
		}}, nil
}

type service struct {
	conn *systemd.Conn
	runc *runc.Runc
	root string
}

// Cleanup is a binary call that cleans up any resources used by the shim when the service crashes
func (s *service) Cleanup(ctx context.Context) (*taskapi.DeleteResponse, error) {
	return &taskapi.DeleteResponse{}, nil
}

func unitName(ns, id string) string {
	return "containerd-" + ns + "-" + id + ".service"
}

// Create a new container
func (s *service) Create(ctx context.Context, r *taskapi.CreateTaskRequest) (_ *taskapi.CreateTaskResponse, retErr error) {
	log.G(ctx).Info("systemd.Create")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Create end")
	}()
	ch := make(chan string, 1)
	runcPath, err := exec.LookPath("runc")
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Join("/run/runc/"), 0711); err != nil {
		return nil, err
	}

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	name := unitName(ns, r.ID)

	pidFile := filepath.Join(s.root, ns, r.ID, "pid")
	execStart := []string{runcPath, "create", "--bundle=" + r.Bundle, "--pid-file", pidFile, r.ID}

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

	defer func() {
		if retErr != nil {
			s.runc.Delete(ctx, r.ID, &runc.DeleteOpts{Force: true})
			if err := s.conn.ResetFailedUnitContext(ctx, name); err != nil {
				log.G(ctx).WithError(err).Info("Failed to reset failed unit")
			}
		}
	}()

	properties := []systemd.Property{
		systemd.PropExecStart(execStart, false),
		systemd.PropType("forking"),
		{Name: "PIDFile", Value: dbus.MakeVariant(pidFile)},
	}
	_, err = s.conn.StartTransientUnitContext(ctx, name, "replace", properties, ch)
	if err != nil {
		if e := s.conn.ResetFailedUnitContext(ctx, name); e == nil {
			_, err2 := s.conn.StartTransientUnitContext(ctx, name, "replace", properties, ch)
			if err2 == nil {
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
	}

	select {
	case <-ctx.Done():
	case <-ch:
	}

	p, err := s.conn.GetServicePropertyContext(ctx, r.ID+".service", "MainPID")
	if err != nil {
		return nil, err
	}
	return &taskapi.CreateTaskResponse{Pid: p.Value.Value().(uint32)}, nil
}

// Start the primary user process inside the container
func (s *service) Start(ctx context.Context, r *taskapi.StartRequest) (_ *taskapi.StartResponse, retErr error) {
	log.G(ctx).Info("systemd.Start")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Start end")
	}()

	if err := s.runc.Start(ctx, r.ID); err != nil {
		return nil, err
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
	if err := s.runc.Delete(ctx, r.ID, &runc.DeleteOpts{Force: true}); err != nil {
		return nil, err
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

// State returns runtime state of a process
func (s *service) State(ctx context.Context, r *taskapi.StateRequest) (_ *taskapi.StateResponse, retErr error) {
	log.G(ctx).Info("systemd.State")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.State end")
	}()

	st, err := s.runc.State(ctx, r.ID)
	if err != nil {
		return nil, err
	}

	return &taskapi.StateResponse{
		ID:     st.ID,
		Pid:    uint32(st.Pid),
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
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	s.conn.KillUnitContext(ctx, unitName(ns, r.ID), int32(r.Signal))
	return &ptypes.Empty{}, nil
}

// Pids returns all pids inside the container
func (s *service) Pids(ctx context.Context, r *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, r *taskapi.CloseIORequest) (*ptypes.Empty, error) {
	return &ptypes.Empty{}, nil
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
func (s *service) Wait(ctx context.Context, r *taskapi.WaitRequest) (_ *taskapi.WaitResponse, retErr error) {
	log.G(ctx).Info("systemd.Wait")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Wait end")
	}()
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.conn.Subscribe(); err != nil {
		return nil, err
	}
	defer s.conn.Unsubscribe()

	name := unitName(ns, r.ID)
	unitCh, errCh := s.conn.SubscribeUnitsCustom(time.Second, 4, func(u1, u2 *systemd.UnitStatus) bool { return *u1 != *u2 }, func(id string) bool {
		return id == name
	})

	st, err := s.runc.State(ctx, r.ID)
	if err == nil {
		if toStatus(st.Status) == task.StatusStopped {
			return &taskapi.WaitResponse{ExitStatus: 0, ExitedAt: time.Now()}, nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-errCh:
			return nil, err
		case units := <-unitCh:
			_, ok := units[name]
			if !ok {
				continue
			}

			st, err = s.runc.State(ctx, r.ID)
			if err != nil {
				continue
			}
			if toStatus(st.Status) == task.StatusStopped {
				return &taskapi.WaitResponse{
					ExitStatus: 0,
					ExitedAt:   time.Now(),
				}, nil
			}
		}
	}
}
