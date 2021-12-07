package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/coreos/go-systemd/unit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Start the primary user process inside the container
func (s *Service) Start(ctx context.Context, r *taskapi.StartRequest) (_ *taskapi.StartResponse, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	ctx, span := StartSpan(ctx, "service.Start", trace.WithAttributes(attribute.String(nsAttr, ns), attribute.String(cIDAttr, r.ID), attribute.String(eIDAttr, r.ExecID)))
	defer func() {
		if retErr != nil {
			retErr = errdefs.ToGRPCf(retErr, "start")
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID).WithField("ns", ns).WithField("execID", r.ExecID))

	p := s.processes.Get(path.Join(ns, r.ID))
	if p == nil {
		return nil, fmt.Errorf("%w: %s", errdefs.ErrNotFound, r.ID)
	}

	defer func() {
		if retErr != nil {
			p.SetState(ctx, pState{ExitCode: 139, ExitedAt: time.Now(), Status: "failed"})
		}
	}()

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
		s.send(ctx, ns, &eventsapi.TaskExecStarted{
			ContainerID: r.ID,
			ExecID:      r.ExecID,
			Pid:         pid,
		})
	} else {
		pid, err = p.Start(ctx)
		if err != nil {
			return nil, err
		}
		s.send(ctx, ns, &eventsapi.TaskStart{
			ContainerID: r.ID,
			Pid:         pid,
		})
	}

	return &taskapi.StartResponse{Pid: pid}, nil
}

func (p *process) runcCmd(cmd []string) ([]string, error) {
	root := []string{p.runc.Command, "--debug=" + strconv.FormatBool(p.runc.Debug), "--systemd-cgroup=" + strconv.FormatBool(p.opts.SystemdCgroup), "--root", p.runc.Root}
	if p.runc.Debug {
		root = append(root, "--log="+p.runc.Log)
	}

	return append(root, cmd...), nil
}

func writeUnit(name string, opts []*unit.UnitOption) error {
	rdr := unit.Serialize(opts)

	f, err := os.Create(filepath.Join("/run/systemd/system", name))
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, rdr); err != nil {
		return err
	}
	return nil
}

func (p *initProcess) startOptions(rcmd []string) ([]*unit.UnitOption, error) {
	const svc = "Service"

	sysctl, err := exec.LookPath("systemctl")
	if err != nil {
		return nil, err
	}

	deleteCmd, err := p.runcCmd([]string{"delete", p.id})
	if err != nil {
		return nil, err
	}

	opts := []*unit.UnitOption{
		unit.NewUnitOption(svc, "Type", p.unitType()),
		unit.NewUnitOption(svc, "PIDFile", p.pidFile()),
		unit.NewUnitOption(svc, "Delegate", "yes"),
		unit.NewUnitOption(svc, "ExecStopPost", "-"+sysctl+" reload "+os.Getenv("UNIT_NAME")),
		unit.NewUnitOption(svc, "ExecStopPost", "-"+strings.Join(deleteCmd, " ")),
	}

	var prefix []string
	if len(p.Rootfs) > 0 {
		if p.noNewNamespace {
			opts = append(opts, unit.NewUnitOption(svc, "ExecStartPre", p.exe+" mount "+p.mountConfigPath()))
			opts = append(opts, unit.NewUnitOption(svc, "ExecStopPost", "-"+p.exe+" unmount "+filepath.Join(p.Bundle, "rootfs")))
		} else {
			// Unfortunately with PrivateMounts we can't use `ExecStartPre` to mount the rootfs b/c it does not share a mount namespace
			// with the main process. Instead we re-exec with `create` subcommand which will mount and exec the main process.
			opts = append(opts, unit.NewUnitOption(svc, "PrivateMounts", "yes"))
			prefix = []string{p.exe, "create", p.mountConfigPath()}
		}
	}

	if p.Terminal || p.opts.Terminal {
		opts = append(opts, unit.NewUnitOption("Service", "ExecStopPost", "-"+sysctl+" stop "+p.ttyUnitName()))
	}

	execStart, err := p.runcCmd(append(rcmd, p.id))
	if err != nil {
		return nil, err
	}
	opts = append(opts, unit.NewUnitOption(svc, "ExecStart", strings.Join(append(prefix, execStart...), " ")))

	if p.Stdin != "" {
		opts = append(opts, unit.NewUnitOption(svc, "StandardInput", "file:"+p.Stdin))
	}
	if p.Stdout != "" {
		opts = append(opts, unit.NewUnitOption(svc, "StandardOutput", "file:"+p.Stdout))
	}
	if !p.Terminal && !p.opts.Terminal && p.Stderr != "" {
		opts = append(opts, unit.NewUnitOption(svc, "StandardError", "file:"+p.Stderr))
	} else {
		opts = append(opts, unit.NewUnitOption(svc, "StandardError", "journal"))
	}

	return opts, nil
}

func (p *execProcess) startOptions() ([]*unit.UnitOption, error) {
	const svc = "Service"

	sysctl, err := exec.LookPath("systemctl")
	if err != nil {
		return nil, err
	}

	opts := []*unit.UnitOption{
		unit.NewUnitOption(svc, "Type", p.unitType()),
		unit.NewUnitOption(svc, "PIDFile", p.pidFile()),
		unit.NewUnitOption(svc, "Delegate", "yes"),
		unit.NewUnitOption(svc, "ExecStopPost", "-"+sysctl+" reload "+os.Getenv("UNIT_NAME")),
		unit.NewUnitOption(svc, "ExecStopPost", "-"+"/bin/cp "+p.pidFile()+" "+p.pidFile()+".exited"),
	}

	if p.Terminal || p.opts.Terminal {
		opts = append(opts, unit.NewUnitOption("Service", "ExecStopPost", "-"+sysctl+" stop "+p.ttyUnitName()))
	}

	cmd := []string{"exec", "--process=" + p.processFilePath(), "--pid-file=" + p.pidFile(), "--detach"}
	if p.Terminal || p.opts.Terminal {
		s, err := p.ttySockPath()
		if err != nil {
			return nil, err
		}
		cmd = append(cmd, "-t")
		cmd = append(cmd, "--console-socket="+s)
	}

	execStart, err := p.runcCmd(append(cmd, p.parent.id))
	if err != nil {
		return nil, err
	}
	opts = append(opts, unit.NewUnitOption(svc, "ExecStart", strings.Join(execStart, " ")))

	if p.Stdin != "" {
		opts = append(opts, unit.NewUnitOption(svc, "StandardInput", "file:"+p.Stdin))
	}
	if p.Stdout != "" {
		opts = append(opts, unit.NewUnitOption(svc, "StandardOutput", "file:"+p.Stdout))
	}
	if !p.Terminal && !p.opts.Terminal && p.Stderr != "" {
		opts = append(opts, unit.NewUnitOption(svc, "StandardError", "file:"+p.Stderr))
	} else {
		opts = append(opts, unit.NewUnitOption(svc, "StandardError", "journal"))
	}

	return opts, nil
}

func (p *process) unitType() string {
	if p.opts.SdNotifyEnable {
		return "notify"
	}
	return "forking"
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
	if err := p.runc.Start(ctx, p.id); err != nil {
		ret := fmt.Errorf("failed runc start: %w", err)
		if p.runc.Debug {
			unitData, err := os.ReadFile("/run/systemd/system/" + p.Name())
			if err == nil {
				ret = fmt.Errorf("%w:\n%s\n%s", ret, p.Name(), unitData)
			}

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
	p.mu.Lock()
	pid = p.state.Pid
	p.mu.Unlock()
	return pid, nil
}

func (p *initProcess) restore(ctx context.Context) (pid uint32, retErr error) {
	if p.Terminal || p.opts.Terminal {
		u, _, err := p.makePty(ctx)
		if err != nil {
			return 0, err
		}
		defer func() {
			if retErr != nil {
				p.systemd.KillUnitContext(ctx, u, int32(syscall.SIGKILL))
			}
		}()
	}
	return p.startUnit(ctx)
}

func (p *execProcess) Start(ctx context.Context) (_ uint32, retErr error) {
	if p.Stdin != "" {
		f, err := os.OpenFile(p.Stdin, os.O_RDWR, 0)
		if err == nil {
			defer f.Close()
		}
	}

	if p.Stdout != "" {
		f, err := os.OpenFile(p.Stdout, os.O_RDWR, 0)
		if err == nil {
			defer f.Close()
		}
	}

	if p.Stderr != "" {
		f, err := os.OpenFile(p.Stderr, os.O_RDWR, 0)
		if err == nil {
			defer f.Close()
		}
	}

	ch := make(chan string, 1)

	if _, err := p.systemd.StartUnitContext(ctx, p.Name(), "replace", ch); err != nil {
		return 0, err
	}

	select {
	case <-ctx.Done():
		p.systemd.KillUnitContext(ctx, p.Name(), int32(syscall.SIGKILL))
	case status := <-ch:
		if status != "done" {
			pidData, err := os.ReadFile(p.pidFile() + ".exited")
			if err == nil && len(pidData) > 0 {
				// TODO: this is a hack to get the pid of the exec process
				// Unfortuantely if the process exited quickly systemd reports this as a "protocol error"
				// We also cannot get the correct status code in this case.
				pid, err := strconv.Atoi(string(pidData))
				if err != nil {
					return 0, fmt.Errorf("could not read pid: %w", err)
				}
				p.mu.Lock()
				p.state.Pid = uint32(pid)
				p.state.ExitedAt = time.Now()
				p.state.ExitCode = 139
				p.mu.Unlock()
				return uint32(pid), nil
			} else {
				log.G(ctx).WithError(err).Warn("Error reading pid file")
			}

			ret := fmt.Errorf("exec failed to start: %s", status)
			if p.runc.Debug {
				unitData, err := os.ReadFile("/run/systemd/system/" + p.Name())
				if err == nil {
					ret = fmt.Errorf("%w:\n%s\n%s", ret, p.Name(), unitData)
				}

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

	pid, err := p.getPid(ctx)
	if err != nil {
		return 0, err
	}

	p.mu.Lock()
	p.state.Pid = pid
	p.mu.Unlock()

	return pid, nil
}