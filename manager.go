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
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/go-runc"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// We always use the same grouping so there is a single shim for all containers
const grouping = "systemd-shim"

var (
	timeZero = time.UnixMicro(0)
)

func New(ctx context.Context, ns, root string) (*Service, error) {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, err
	}

	return &Service{
		conn: conn,
		root: root,
		ns:   ns,
		runc: &runc.Runc{
			// Root:          filepath.Join(root, "runc"),
			SystemdCgroup: true,
			PdeathSignal:  syscall.SIGKILL,
		}}, nil
}

type Service struct {
	conn *systemd.Conn
	runc *runc.Runc
	root string
	ns   string
}

// Cleanup is a binary call that cleans up any resources used by the shim when the Service crashes
func (s *Service) Cleanup(ctx context.Context) (*taskapi.DeleteResponse, error) {
	return &taskapi.DeleteResponse{}, nil
}

func unitName(ns, id, t string) string {
	if t == "" {
		t = "service"
	}
	return "containerd-" + ns + "-" + id + "." + t
}

func runcName(ns, id string) string {
	return ns + "-" + id
}

// Create a new container
func (s *Service) Create(ctx context.Context, r *taskapi.CreateTaskRequest) (_ *taskapi.CreateTaskResponse, retErr error) {
	name := unitName(s.ns, r.ID, "service")
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", s.ns).WithField("unitName", name))

	log.G(ctx).Info("systemd.Create started")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Create end")
	}()
	ch := make(chan string, 1)
	runcPath, err := exec.LookPath("runc")
	if err != nil {
		return nil, err
	}

	// See https://github.com/opencontainers/runc/issues/3202
	if err := os.MkdirAll("/run/runc", 0700); err != nil {
		return nil, err
	}

	pidFile := filepath.Join(s.root, r.ID, "pid")

	execStart := []string{runcPath, "create", "--bundle=" + r.Bundle, "--pid-file", pidFile, runcName(s.ns, r.ID)}

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
		if err := os.Mkdir(rootfs, 0700); err != nil && !os.IsExist(err) {
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

	// TODO: TTY

	// TODO: I'd like to use either the "StandardInput/Output/Error" properties here, however systemd is rejecting them.
	//   Assuming this is because they are not supported for transient units (though code and docs seems to indicate otherwise)

	var envs []string
	if r.Stdin != "" {
		envs = append(envs, "STDIN_PATH="+r.Stdin)
	}
	if r.Stdout != "" {
		envs = append(envs, "STDOUT_PATH="+r.Stdout)
	}
	if r.Stderr != "" {
		envs = append(envs, "STDERR_PATH="+r.Stdout)
	}

	if len(envs) > 0 {
		// Hack to inject inject fifo's as stdio for tthe container process
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		execStart = append([]string{exe, "run"}, execStart...)
	}

	properties := []systemd.Property{
		systemd.PropExecStart(execStart, false),
		systemd.PropType("forking"),
		{Name: "PIDFile", Value: dbus.MakeVariant(pidFile)},
	}
	if len(envs) > 0 {
		properties = append(properties, systemd.Property{Name: "Environment", Value: dbus.MakeVariant(envs)})
	}

	defer func() {
		if retErr != nil {
			s.runc.Delete(ctx, runcName(s.ns, r.ID), &runc.DeleteOpts{Force: true})
			s.conn.ResetFailedUnitContext(ctx, name)
		}
	}()

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

	p, err := s.conn.GetServicePropertyContext(ctx, name, "MainPID")
	if err != nil {
		return nil, err
	}
	return &taskapi.CreateTaskResponse{Pid: p.Value.Value().(uint32)}, nil
}

// Start the primary user process inside the container
func (s *Service) Start(ctx context.Context, r *taskapi.StartRequest) (_ *taskapi.StartResponse, retErr error) {
	defer func() {
		if retErr != nil {
			retErr = errors.Wrap(retErr, "start")
		}
	}()

	name := unitName(s.ns, r.ID, "service")

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", s.ns).WithField("unitName", name))

	log.G(ctx).Info("systemd.Start")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Start end")
	}()

	if err := s.runc.Start(ctx, runcName(s.ns, r.ID)); err != nil {
		return nil, err
	}

	p, err := s.conn.GetServicePropertyContext(ctx, name, "MainPID")
	if err != nil {
		return nil, err
	}

	return &taskapi.StartResponse{Pid: p.Value.Value().(uint32)}, nil
}

// Delete a process or container
func (s *Service) Delete(ctx context.Context, r *taskapi.DeleteRequest) (_ *taskapi.DeleteResponse, retErr error) {
	log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", s.ns))

	log.G(ctx).Info("systemd.Delete begin")
	name := unitName(s.ns, r.ID, "service")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.Delete end")
	}()

	defer func() {
		mount.UnmountAll(filepath.Join(s.root, r.ID, "rootfs"), 0)
	}()
	if err := s.runc.Delete(ctx, runcName(s.ns, r.ID), &runc.DeleteOpts{Force: true}); err != nil {
		return nil, err
	}

	var st execState
	if err := s.getState(ctx, name, &st); err != nil && !errdefs.IsNotFound(err) {
		log.G(ctx).WithError(err).Error("Error getting unit state")
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("statusCode", st.ExitCode))

	return &taskapi.DeleteResponse{
		Pid:        st.Pid,
		ExitStatus: st.ExitCode,
		ExitedAt:   st.ExitedAt,
	}, nil
}

// Exec an additional process inside the container
func (s *Service) Exec(ctx context.Context, r *taskapi.ExecProcessRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// ResizePty of a process
func (s *Service) ResizePty(ctx context.Context, r *taskapi.ResizePtyRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// State returns runtime state of a process
func (s *Service) State(ctx context.Context, r *taskapi.StateRequest) (_ *taskapi.StateResponse, retErr error) {
	log.G(ctx).Info("systemd.State")
	defer func() {
		log.G(ctx).WithError(retErr).Info("systemd.State end")
	}()

	st, err := s.runc.State(ctx, runcName(s.ns, r.ID))
	if err != nil {
		return nil, err
	}

	status := toStatus(st.Status)
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("status", status))

	resp := &taskapi.StateResponse{
		ID:     st.ID,
		Pid:    uint32(st.Pid),
		Status: status,
		Bundle: st.Bundle,
	}

	if status == task.StatusStopped {
		var sdSt execState
		if err := s.getState(ctx, unitName(s.ns, r.ID, "service"), &sdSt); err != nil {
			return nil, err
		}

		resp.ExitStatus = uint32(sdSt.ExitCode)
		resp.ExitedAt = sdSt.ExitedAt
		ctx = log.WithLogger(ctx, log.G(ctx).WithField("exitStatus", sdSt.ExitCode))
	}

	return resp, nil
}

// Pause the container
func (s *Service) Pause(ctx context.Context, r *taskapi.PauseRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Resume the container
func (s *Service) Resume(ctx context.Context, r *taskapi.ResumeRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Kill a process
func (s *Service) Kill(ctx context.Context, r *taskapi.KillRequest) (*ptypes.Empty, error) {
	s.conn.KillUnitContext(ctx, unitName(s.ns, r.ID, ""), int32(r.Signal))
	return &ptypes.Empty{}, nil
}

// Pids returns all pids inside the container
func (s *Service) Pids(ctx context.Context, r *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// CloseIO of a process
func (s *Service) CloseIO(ctx context.Context, r *taskapi.CloseIORequest) (*ptypes.Empty, error) {
	return &ptypes.Empty{}, nil
}

// Checkpoint the container
func (s *Service) Checkpoint(ctx context.Context, r *taskapi.CheckpointTaskRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Connect returns shim information of the underlying Service
func (s *Service) Connect(ctx context.Context, r *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// Shutdown is called after the underlying resources of the shim are cleaned up and the Service can be stopped
func (s *Service) Shutdown(ctx context.Context, r *taskapi.ShutdownRequest) (*ptypes.Empty, error) {
	return &ptypes.Empty{}, nil
}

// Stats returns container level system stats for a container and its processes
func (s *Service) Stats(ctx context.Context, r *taskapi.StatsRequest) (*taskapi.StatsResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// Update the live container
func (s *Service) Update(ctx context.Context, r *taskapi.UpdateTaskRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Wait for a process to exit
func (s *Service) Wait(ctx context.Context, r *taskapi.WaitRequest) (retResp *taskapi.WaitResponse, retErr error) {
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(logrus.Fields{
		"id":        r.ID,
		"ns":        s.ns,
		"apiAction": "wait",
	}))

	log.G(ctx).Info("Start")
	defer func() {
		log.G(ctx).WithError(retErr).WithField("exitedAt", retResp.ExitedAt).Info("End")
	}()

	if err := s.conn.Subscribe(); err != nil {
		return nil, err
	}

	name := unitName(s.ns, r.ID, "service")
	unitCh, errCh := s.conn.SubscribeUnitsCustom(time.Second, 10, func(u1, u2 *systemd.UnitStatus) bool { return u1.ActiveState != u2.ActiveState }, func(id string) bool {
		return id != name
	})

	var (
		st execState
	)

	if err := s.getState(ctx, name, &st); err != nil {
		log.G(ctx).WithError(err).Error("failed to get state")
	}

	if st.ExitedAt.After(timeZero) {
		return &taskapi.WaitResponse{
			ExitStatus: st.ExitCode,
			ExitedAt:   st.ExitedAt,
		}, nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-errCh:
			return nil, err
		case units := <-unitCh:
			log.G(ctx).Debugf("Got %d state updates", len(units))
			_, ok := units[name]
			if !ok {
				log.G(ctx).Debug("Our unit is not in the state changes")
				continue
			}

			if err := s.getState(ctx, name, &st); err != nil {
				log.G(ctx).WithError(err).Error("failed to get state")
			}

			if st.ExitedAt.After(timeZero) {
				return &taskapi.WaitResponse{
					ExitStatus: st.ExitCode,
					ExitedAt:   st.ExitedAt,
				}, nil
			}
		}
	}
}
