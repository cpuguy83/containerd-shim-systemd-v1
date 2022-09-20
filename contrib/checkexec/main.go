package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

func main() {
	if err := do(); err != nil {
		panic(err)
	}
}

func do() error {
	baseCtx := namespaces.WithNamespace(context.Background(), "default")
	ctx, cancel := signal.NotifyContext(baseCtx, os.Interrupt)
	defer cancel()

	var args []string
	if len(os.Args) > 1 {
		args = os.Args[1:]
		if args[0][0] != '/' {
			args[0] = "/" + args[0]
		}
	} else {
		args = []string{"/bin/sh"}
	}

	client, err := containerd.New("/run/containerd/containerd.sock")
	if err != nil {
		return fmt.Errorf("failed to create containerd client: %v", err)
	}

	if c, _ := client.LoadContainer(ctx, "test"); c != nil {
		c.Delete(ctx, containerd.WithSnapshotCleanup)
	}

	img, err := client.Pull(ctx, "docker.io/library/busybox:latest", containerd.WithPullUnpack)
	if err != nil {
		return fmt.Errorf("failed to pull busybox: %v", err)
	}

	c, err := client.NewContainer(ctx, "test",
		containerd.WithImage(img),
		containerd.WithNewSnapshot("test", img),
		containerd.WithRuntime("io.containerd.systemd.v1", nil),
		containerd.WithNewSpec(
			oci.WithImageConfig(img),
			oci.WithProcessArgs("/bin/top"),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to create container: %v", err)
	}

	defer func() {
		if err := c.Delete(baseCtx, containerd.WithSnapshotCleanup); err != nil {
			log.Println("container delete:", err.Error())
		}
	}()

	t, err := c.NewTask(ctx, cio.NullIO)
	if err != nil {
		return fmt.Errorf("failed to create task: %v", err)
	}
	defer func() {
		if err := t.Kill(baseCtx, syscall.SIGKILL); err != nil {
			log.Println("task kill:", err.Error())
		}
		ch, err := t.Wait(ctx)
		if err != nil {
			log.Println("task wait", err.Error())
		}
		if ch != nil {
			es := <-ch
			ec, et, err := es.Result()
			log.Println("task wait:", err, ec, et)
		}
		if _, err := t.Delete(baseCtx); err != nil {
			log.Println("task delete", err.Error())
		}
	}()

	p, err := t.Exec(ctx, "test-exec", &specs.Process{
		Args: args,
		Cwd:  "/",
	}, cio.NullIO)
	if err != nil {
		return fmt.Errorf("exec failed: %v", err)
	}
	defer func() {
		if _, err := p.Delete(baseCtx); err != nil {
			log.Println("exec deletE", err.Error())
		}
	}()

	if err := p.Start(ctx); err != nil {
		return fmt.Errorf("exec start failed: %w", err)
	}

	if err := p.Kill(baseCtx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
		log.Println("exec kill:", err.Error())
	}
	ch, err := p.Wait(ctx)
	if err != nil {
		log.Println("exec wait:", err.Error())
	}
	st := <-ch
	ec, et, err := st.Result()
	log.Println("exec wait:", err, ec, et)

	return nil
}
