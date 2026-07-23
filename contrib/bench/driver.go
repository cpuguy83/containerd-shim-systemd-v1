package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/console"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// driver drives container/exec lifecycles through the containerd client. The
// only thing that varies between the two runtimes under test is the runtime
// string passed to WithRuntime, so every measured path is identical otherwise.
type driver struct {
	client *containerd.Client
	image  containerd.Image
	cfg    Config
	seq    atomic.Int64
}

func newDriver(ctx context.Context, cfg Config) (*driver, error) {
	client, err := containerd.New(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("connect containerd at %s: %w", cfg.Address, err)
	}
	img, err := client.Pull(ctx, cfg.Image, containerd.WithPullUnpack)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", cfg.Image, err)
	}
	return &driver{client: client, image: img, cfg: cfg}, nil
}

func (d *driver) close() error { return d.client.Close() }

func (d *driver) uid(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, d.seq.Add(1))
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}

// ---- IO ----------------------------------------------------------------

// newIO returns a cio.Creator for the workload. For TTY workloads it allocates
// a real pty (mirroring how ctr passes the controlling terminal) and drains the
// slave so the shim's copier never blocks. The returned cleanup closes the pty.
func newIO(tty bool) (cio.Creator, func(), error) {
	if !tty {
		return cio.NullIO, func() {}, nil
	}
	master, slavePath, err := console.NewPty()
	if err != nil {
		return nil, nil, fmt.Errorf("allocate pty: %w", err)
	}
	slave, err := os.OpenFile(slavePath, os.O_RDWR, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open pty slave: %w", err)
	}
	go io.Copy(io.Discard, slave)
	creator := cio.NewCreator(cio.WithStreams(master, master, nil), cio.WithTerminal)
	cleanup := func() {
		slave.Close()
		master.Close()
	}
	return creator, cleanup, nil
}

func (d *driver) newContainer(ctx context.Context, rt, id string, tty bool, args []string) (containerd.Container, error) {
	specOpts := []oci.SpecOpts{oci.WithImageConfig(d.image), oci.WithProcessArgs(args...)}
	if tty {
		specOpts = append(specOpts, oci.WithTTY)
	}
	return d.client.NewContainer(ctx, id,
		containerd.WithImage(d.image),
		containerd.WithNewSnapshot(id, d.image),
		containerd.WithRuntime(rt, nil),
		containerd.WithNewSpec(specOpts...),
	)
}

// containerLifecycle runs one short-lived container end to end and returns the
// per-op latencies in ms. Cleanup is best-effort on every path.
func (d *driver) containerLifecycle(ctx context.Context, rt string, tty bool) (map[string]float64, error) {
	ops := map[string]float64{}
	id := d.uid("ctr")

	ioc, cleanup, err := newIO(tty)
	if err != nil {
		return ops, err
	}
	defer cleanup()

	t0 := time.Now()
	c, err := d.newContainer(ctx, rt, id, tty, []string{"/bin/true"})
	ops["container_new"] = msSince(t0)
	if err != nil {
		return ops, err
	}
	deleted := false
	defer func() {
		if !deleted {
			c.Delete(context.Background(), containerd.WithSnapshotCleanup)
		}
	}()

	t1 := time.Now()
	task, err := c.NewTask(ctx, ioc)
	ops["create"] = msSince(t1)
	if err != nil {
		return ops, err
	}
	taskDeleted := false
	defer func() {
		if !taskDeleted {
			task.Kill(context.Background(), 9)
			task.Delete(context.Background())
		}
	}()

	statusC, err := task.Wait(ctx)
	if err != nil {
		return ops, err
	}

	t2 := time.Now()
	if err := task.Start(ctx); err != nil {
		return ops, err
	}
	ops["start"] = msSince(t2)

	select {
	case <-statusC:
		ops["run"] = msSince(t2)
	case <-ctx.Done():
		return ops, ctx.Err()
	}

	t3 := time.Now()
	if _, err := task.Delete(ctx); err != nil {
		return ops, err
	}
	taskDeleted = true
	ops["delete"] = msSince(t3)

	t4 := time.Now()
	if err := c.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		return ops, err
	}
	deleted = true
	ops["container_delete"] = msSince(t4)

	ops["total"] = ops["create"] + ops["start"] + ops["run"] + ops["delete"]
	return ops, nil
}

// longContainer creates and starts a long-running container (sleep) and returns
// a teardown func. Used as the target of exec/status scenarios.
func (d *driver) longContainer(ctx context.Context, rt, id string) (containerd.Task, func(), error) {
	c, err := d.newContainer(ctx, rt, id, false, []string{"/bin/sleep", "3600"})
	if err != nil {
		return nil, nil, err
	}
	task, err := c.NewTask(ctx, cio.NullIO)
	if err != nil {
		c.Delete(context.Background(), containerd.WithSnapshotCleanup)
		return nil, nil, err
	}
	statusC, err := task.Wait(ctx)
	if err != nil {
		task.Delete(context.Background())
		c.Delete(context.Background(), containerd.WithSnapshotCleanup)
		return nil, nil, err
	}
	if err := task.Start(ctx); err != nil {
		task.Delete(context.Background())
		c.Delete(context.Background(), containerd.WithSnapshotCleanup)
		return nil, nil, err
	}
	teardown := func() {
		bg := namespaces.WithNamespace(context.Background(), d.cfg.Namespace)
		task.Kill(bg, 9)
		select {
		case <-statusC:
		case <-time.After(5 * time.Second):
		}
		task.Delete(bg)
		c.Delete(bg, containerd.WithSnapshotCleanup)
	}
	return task, teardown, nil
}

// execOnce runs one short-lived exec against an existing task.
func (d *driver) execOnce(ctx context.Context, task containerd.Task, tty bool) (map[string]float64, error) {
	ops := map[string]float64{}
	ioc, cleanup, err := newIO(tty)
	if err != nil {
		return ops, err
	}
	defer cleanup()

	spec := &specs.Process{Args: []string{"/bin/true"}, Cwd: "/", Terminal: tty}
	execID := d.uid("exec")

	t0 := time.Now()
	p, err := task.Exec(ctx, execID, spec, ioc)
	ops["exec_create"] = msSince(t0)
	if err != nil {
		return ops, err
	}
	deleted := false
	defer func() {
		if !deleted {
			p.Delete(context.Background())
		}
	}()

	statusC, err := p.Wait(ctx)
	if err != nil {
		return ops, err
	}

	t1 := time.Now()
	if err := p.Start(ctx); err != nil {
		return ops, err
	}
	ops["start"] = msSince(t1)

	select {
	case <-statusC:
		ops["run"] = msSince(t1)
	case <-ctx.Done():
		return ops, ctx.Err()
	}

	t2 := time.Now()
	if _, err := p.Delete(ctx); err != nil {
		return ops, err
	}
	deleted = true
	ops["delete"] = msSince(t2)

	ops["total"] = ops["exec_create"] + ops["start"] + ops["run"] + ops["delete"]
	return ops, nil
}

// runConcurrentN runs fn total times with at most parallel in flight, returning
// the merged per-op latency samples, the error count, and total wall time.
func runConcurrentN(total, parallel int, fn func() (map[string]float64, error)) (map[string][]float64, int, time.Duration) {
	if parallel < 1 {
		parallel = 1
	}
	sem := make(chan struct{}, parallel)
	var (
		mu   sync.Mutex
		raw  = map[string][]float64{}
		errs int
		wg   sync.WaitGroup
	)

	start := time.Now()
	for range total {
		sem <- struct{}{}

		wg.Go(func() {
			defer func() { <-sem }()
			ops, err := fn()
			mu.Lock()
			if err != nil {
				errs++
			}
			for k, v := range ops {
				raw[k] = append(raw[k], v)
			}
			mu.Unlock()
		})
	}

	wg.Wait()
	return raw, errs, time.Since(start)
}
