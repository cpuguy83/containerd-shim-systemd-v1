package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"
)

// Start the primary user process inside the container
func (s *Service) Start(ctx context.Context, r *taskapi.StartRequest) (_ *taskapi.StartResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Start", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID), attribute.String(eIDAttr, r.ExecID)))
	defer func() {
		if retErr != nil {
			retErr = errgrpc.ToGRPC(fmt.Errorf("start: %w", retErr))
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns).WithField("execID", r.ExecID))

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("%w: %s", errdefs.ErrNotFound, r.ID)
	}

	ctx = WithShimLog(ctx, p.LogWriter())

	var pid uint32
	if r.ExecID != "" {
		ep := p.(*initProcess).execs.Get(r.ExecID)
		if ep == nil {
			return nil, fmt.Errorf("exec %s: %w", r.ExecID, errdefs.ErrNotFound)
		}
		pid, err = ep.Start(ctx)
		if err != nil {
			s.units.Delete(ep)
			return nil, err
		}
		ep.(*execProcess).markStarted()
		ep.SetState(ctx, pState{Pid: pid, Status: "running"})
		s.send(ctx, ns, &eventsapi.TaskExecStarted{
			ContainerID: r.ID,
			ExecID:      r.ExecID,
			Pid:         pid,
		})
		ep.(*execProcess).markStartEventPublished()
	} else {
		pid, err = p.Start(ctx)
		if err != nil {
			return nil, err
		}
		p.(*initProcess).markStarted()
		p.SetState(ctx, pState{Pid: pid, Status: "running"})
		s.send(ctx, ns, &eventsapi.TaskStart{
			ContainerID: r.ID,
			Pid:         pid,
		})
		p.(*initProcess).markStartEventPublished()
	}

	return &taskapi.StartResponse{Pid: pid}, nil
}

func (p *process) runcCmd(cmd []string) ([]string, error) {
	root := []string{p.runc.Command, "--debug=" + strconv.FormatBool(p.runc.Debug), "--systemd-cgroup=" + strconv.FormatBool(p.opts.SystemdCgroup), "--root", p.runc.Root}
	if p.runc.Debug {
		root = append(root, "--log", p.runc.Log)
	}

	return append(root, cmd...), nil
}

// execCommand mirrors systemd's transient exec tuple `(sasb)`: the binary path,
// the full argv (starting with argv[0]), and whether a non-zero exit is ignored
// (the unit-file "-" prefix). go-systemd only ships a helper for ExecStart, so
// this covers ExecStartPre/ExecStopPost uniformly too.
type execCommand struct {
	Path       string
	Args       []string
	IgnoreFail bool
}

// propExecCommand builds an Exec* property (ExecStart, ExecStartPre,
// ExecStopPost, ...) from one or more exec tuples. The value marshals to the
// `a(sasb)` type systemd expects for transient units.
func propExecCommand(name string, cmds ...execCommand) systemd.Property {
	return systemd.Property{Name: name, Value: dbus.MakeVariant(cmds)}
}

// formatUnitProperties renders transient-unit properties for debug output. The
// shim no longer writes a unit file, so this stands in for reading one back when
// reporting a failed start.
func formatUnitProperties(props []systemd.Property) string {
	var b strings.Builder
	for _, p := range props {
		fmt.Fprintf(&b, "%s=%v\n", p.Name, p.Value.Value())
	}
	return b.String()
}

func (p *initProcess) startProperties(rcmd []string) ([]systemd.Property, error) {
	env := []string{
		"PIDFILE=" + p.pidFile(),
		// Set the container stdio fifos as env vars so they are only used for the
		// container stdio, not the other commands we run. Otherwise we can hit
		// cases where the client has closed the fifo and our Pre/Post commands
		// hang. The create re-exec opens them just before executing runc.
		"STDIN_FIFO=" + p.Stdin,
		"STDOUT_FIFO=" + p.Stdout,
		"STDERR_FIFO=" + p.Stderr,
		"UNIT_NAME=" + p.Name(),
		"EXIT_STATE_PATH=" + p.exitStatePath(),
	}
	if p.shimCgroup != "" {
		env = append(env, "SHIM_CGROUP="+p.shimCgroup)
	}

	props := []systemd.Property{
		systemd.PropType(p.unitType()),
		systemd.PropRemainAfterExit(false),
		{Name: "WorkingDirectory", Value: dbus.MakeVariant(p.Bundle)},
		{Name: "PIDFile", Value: dbus.MakeVariant(p.pidFile())},
		{Name: "Delegate", Value: dbus.MakeVariant(true)},
	}

	prefix := []string{p.exe, "--debug=" + strconv.FormatBool(p.runc.Debug), "--bundle=" + p.Bundle, "create"}
	if len(p.Rootfs) > 0 {
		if p.noNewNamespace {
			props = append(props,
				propExecCommand("ExecStartPre", execCommand{Path: p.exe, Args: []string{p.exe, "mount", p.mountConfigPath()}}),
				propExecCommand("ExecStopPost", execCommand{Path: p.exe, Args: []string{p.exe, "unmount", filepath.Join(p.Bundle, "rootfs")}, IgnoreFail: true}),
			)
		} else {
			// Unfortunately with PrivateMounts we can't use `ExecStartPre` to mount the rootfs b/c it does not share a mount namespace
			// with the main process. Instead we re-exec with `create` subcommand which will mount and exec the main process.
			props = append(props, systemd.Property{Name: "PrivateMounts", Value: dbus.MakeVariant(true)})
			prefix = append(prefix, "--mounts="+p.mountConfigPath())
		}
	}

	if p.Terminal || p.opts.Terminal {
		prefix = append(prefix, "--tty")
	}

	execStart, err := p.runcCmd(append(rcmd, p.id))
	if err != nil {
		return nil, err
	}
	argv := append(prefix, execStart...)
	props = append(props,
		propExecCommand("ExecStart", execCommand{Path: argv[0], Args: argv}),
		systemd.Property{Name: "Environment", Value: dbus.MakeVariant(env)},
	)

	return props, nil
}

func (p *execProcess) startProperties() ([]systemd.Property, error) {
	env := []string{
		"PIDFILE=" + p.pidFile(),
		// See the note in initProcess.startProperties on why stdio fifos are env vars.
		"STDIN_FIFO=" + p.Stdin,
		"STDOUT_FIFO=" + p.Stdout,
		"STDERR_FIFO=" + p.Stderr,
		"UNIT_NAME=" + p.Name(),
		"EXIT_STATE_PATH=" + p.exitStatePath(),
	}
	if p.shimCgroup != "" {
		env = append(env, "SHIM_CGROUP="+p.shimCgroup)
	}

	props := []systemd.Property{
		systemd.PropType("notify"),
		systemd.PropRemainAfterExit(false),
		{Name: "WorkingDirectory", Value: dbus.MakeVariant(p.parent.Bundle)},
		{Name: "PIDFile", Value: dbus.MakeVariant(p.pidFile())},
		{Name: "GuessMainPID", Value: dbus.MakeVariant(true)},
		{Name: "Delegate", Value: dbus.MakeVariant(true)},
	}

	prefix := []string{p.exe, "--debug=" + strconv.FormatBool(p.runc.Debug), "--bundle=" + p.parent.Bundle, "create"}

	cmd := []string{"exec", "--process", p.processFilePath(), "--pid-file=" + p.pidFile(), "--detach"}
	if p.Terminal || p.opts.Terminal {
		s, err := p.ttySockPath()
		if err != nil {
			return nil, err
		}

		cmd = append(cmd, "-t", "--console-socket="+s)
		prefix = append(prefix, "--tty")
	}

	execStart, err := p.runcCmd(append(cmd, p.parent.id))
	if err != nil {
		return nil, err
	}
	argv := append(prefix, execStart...)
	props = append(props,
		propExecCommand("ExecStart", execCommand{Path: argv[0], Args: argv}),
		systemd.Property{Name: "Environment", Value: dbus.MakeVariant(env)},
	)

	return props, nil
}

func (p *process) unitType() string {
	if p.opts.SdNotifyEnable {
		return "notify"
	}
	return "forking"
}

// abandonStartedUnit tears down a unit whose process was deleted while it was
// starting. The delete already ran its teardown against a unit that did not
// exist yet, so it is this caller's job to stop the unit and release the
// reference it just took, or both would outlive the process untracked.
func (p *process) abandonStartedUnit(ctx context.Context, name string) {
	// Stopping gets its own budget. Releasing the reference is the part that must
	// not be skipped, so a unit that refuses to stop must not be able to spend the
	// time the release needs.
	stopCtx, cancelStop := cleanupContext(ctx)
	ch := make(chan string, 1)
	if _, err := p.systemd.StopUnitContext(stopCtx, name, "replace", ch); err != nil {
		log.G(ctx).WithField("unit", name).WithError(err).Info("Failed to stop unit for a deleted process")
	} else {
		select {
		case <-stopCtx.Done():
		case <-ch:
		}
	}
	cancelStop()

	killCtx, cancelKill := cleanupContext(ctx)
	p.systemd.KillUnitContext(killCtx, name, int32(syscall.SIGKILL))
	cancelKill()

	// The release gets the last, unshared budget: it is the one step whose
	// failure is unrecoverable, so no earlier call may spend its time.
	relCtx, cancel := cleanupContext(ctx)
	defer cancel()
	p.releaseUnit(relCtx, name)
	p.systemd.ResetFailedUnitContext(relCtx, name)
}

func (p *initProcess) Start(ctx context.Context) (pid uint32, retErr error) {
	ctx, span := StartSpan(ctx, "InitProcess.Start")
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.SetAttributes(attribute.Int("pid", int(pid)))
		span.End()
	}()

	if p.checkpoint != "" {
		return p.restore(ctx)
	}

	if p.ProcessState().Exited() {
		return 0, fmt.Errorf("process has already exited: %s: %w", p.ProcessState(), errdefs.ErrFailedPrecondition)
	}

	if err := p.runc.Start(ctx, p.id); err != nil {
		log.G(ctx).WithError(err).Error("Error calling runc start")
		ret := fmt.Errorf("failed runc start: %w", err)

		if err := p.LoadState(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("Error loading process state")
		}
		if !p.ProcessState().Exited() {
			log.G(ctx).Debug("runc start failed but process is still running, sending sigkill")
			p.systemd.KillUnitContext(ctx, p.Name(), int32(unix.SIGKILL))
			if err := p.LoadState(ctx); err != nil {
				log.G(ctx).WithError(err).Debug("Error loading process state")
			}

			if !p.ProcessState().Exited() {
				p.SetState(ctx, pState{
					Pid:      p.Pid(),
					ExitCode: 255,
					ExitedAt: time.Now(),
					Status:   "failed",
				})
			}
		}
		p.cond.Broadcast()

		if p.runc.Debug {
			ret = fmt.Errorf("%w:\n%s\n%s", ret, p.Name(), formatUnitProperties(p.unitProps))

			processData, err := os.ReadFile(filepath.Join(p.Bundle, "config.json"))
			if err == nil {
				ret = fmt.Errorf("%w:\nprocess.json:\n%s", ret, string(processData))
			}

			debug, err := os.ReadFile(p.runc.Log)
			if err == nil {
				ret = fmt.Errorf("%w:\nrunc debug:\n%s", ret, string(debug))
			} else {
				log.G(ctx).WithError(err).Warn("Error opening runc debug log")
			}
		}
		return 0, ret
	}

	for p.Pid() == 0 && !p.ProcessState().Exited() {
		select {
		case <-ctx.Done():
		default:
		}

		if err := p.LoadState(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("Error loading process state")
		}
	}

	pid = p.Pid()
	return pid, nil
}

func (p *initProcess) restore(ctx context.Context) (pid uint32, retErr error) {
	if p.Terminal || p.opts.Terminal {
		sockPath, err := p.ttySockPath()
		if err != nil {
			return 0, err
		}
		u, _, err := p.makePty(ctx, sockPath)
		if err != nil {
			return 0, err
		}
		defer func() {
			if retErr != nil {
				cleanupCtx, cancel := cleanupContext(ctx)
				defer cancel()
				p.systemd.KillUnitContext(cleanupCtx, u, int32(syscall.SIGKILL))
			}
		}()
	}
	// startUnit can leave a unit behind even when it ultimately fails, so check on
	// every return rather than only on success: a delete racing this start is
	// already untracked and would never release it.
	defer func() {
		if !p.isDeleting() {
			return
		}
		p.abandonStartedUnit(ctx, p.Name())
		pid = 0
		if retErr == nil {
			retErr = fmt.Errorf("task %s was deleted while starting: %w", p.id, errdefs.ErrFailedPrecondition)
		}
	}()

	pid, err := p.startUnit(ctx)
	if err != nil {
		return 0, err
	}
	return pid, nil
}

func (p *execProcess) Start(ctx context.Context) (pid uint32, retErr error) {
	if !p.parent.ProcessState().Started() {
		p.parent.LoadState(ctx)
		if !p.parent.ProcessState().Started() {
			return 0, fmt.Errorf("%w: container is not started", errdefs.ErrFailedPrecondition)
		}
	}

	if p.Terminal || p.opts.Terminal {
		sockPath, err := p.ttySockPath()
		if err != nil {
			return 0, err
		}
		u, _, err := p.makePty(ctx, sockPath)
		if err != nil {
			return 0, err
		}

		defer func() {
			if retErr != nil {
				cleanupCtx, cancel := cleanupContext(ctx)
				defer cancel()
				p.systemd.KillUnitContext(cleanupCtx, u, int32(syscall.SIGKILL))
			}
		}()
	}

	props, err := p.startProperties()
	if err != nil {
		return 0, err
	}

	p.clearRecordedSystemdExitState()

	// Installed before the unit is created, not after: StartTransientUnit can
	// fail ambiguously (a lost or cancelled reply) with systemd having already
	// created the unit and applied AddRef, so even its error paths have to hand
	// the reference back if a delete raced us here. The delete ran its teardown
	// against a unit that did not exist yet, and the exec is untracked by now, so
	// nothing else would ever release it.
	defer func() {
		if !p.isDeleting() {
			return
		}
		p.abandonStartedUnit(ctx, p.Name())
		pid = 0
		if retErr == nil {
			retErr = fmt.Errorf("exec %s was deleted while starting: %w", p.execID, errdefs.ErrFailedPrecondition)
		}
	}()

	ch := make(chan string, 1)
	if err := p.startTransientUnit(ctx, p.Name(), "replace", props, ch); err != nil {
		// A prior attempt may have left a failed transient unit of the same
		// name that systemd has not yet garbage-collected; reset it and retry.
		if e := p.systemd.ResetFailedUnitContext(ctx, p.Name()); e != nil {
			log.G(ctx).WithField("unit", p.Name()).WithError(e).Warn("Error resetting failed unit")
		} else {
			ch = make(chan string, 1)
			err = p.startTransientUnit(ctx, p.Name(), "replace", props, ch)
		}
		if err != nil {
			return 0, err
		}
	}

	select {
	case <-ctx.Done():
		log.G(ctx).WithError(ctx.Err()).Warn("start: context cancelled, killing exec unit")
		killCtx, cancel := cleanupContext(ctx)
		defer cancel()
		p.systemd.KillUnitContext(killCtx, p.Name(), int32(syscall.SIGKILL))
	case status := <-ch:
		if status != "done" {
			if err := p.LoadState(ctx); err != nil {
				log.G(ctx).WithError(err).Warn("Error loading process state")
			}

			if !p.ProcessState().Exited() {
				log.G(ctx).Error("Start failed but process is not in exited state")
				break
			}

			if p.ProcessState().ExitCode != 255 {
				break
			}

			ret := fmt.Errorf("error starting exec process")
			if p.runc.Debug {
				ret = fmt.Errorf("%w:\n%s\n%s", ret, p.Name(), formatUnitProperties(props))

				processData, err := os.ReadFile(p.processFilePath())
				if err == nil {
					ret = fmt.Errorf("%w:\nprocess.json:\n%s", ret, string(processData))
				}

				debug, err := os.ReadFile(p.runc.Log)
				if err == nil {
					ret = fmt.Errorf("%w:\nrunc debug:\n%s", ret, string(debug))
				} else {
					log.G(ctx).WithError(err).Warn("Error opening runc debug log")
				}
			}
			return 0, ret
		}
	}

	p.LoadState(ctx)

	if p.ProcessState().Status == exitedInit {
		ret := fmt.Errorf("error starting exec process")
		if p.runc.Debug {
			debug, err := os.ReadFile(p.runc.Log)
			if err == nil {
				ret = fmt.Errorf("%w:\nrunc debug:\n%s", ret, string(debug))
			}
		}
		return 0, ret
	}

	pid, err = p.getPid(ctx)
	if err != nil {
		return 0, err
	}

	p.mu.Lock()
	p.state.Pid = pid
	p.mu.Unlock()

	return pid, nil
}
