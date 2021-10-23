package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/go-runc"
	"github.com/coreos/go-systemd/v22/activation"
	"github.com/gogo/protobuf/proto"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func newCtx() (context.Context, context.CancelFunc) {
	sig := make(chan os.Signal, 1)

	logrus.SetReportCaller(true)
	logrus.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case s := <-sig:
				switch s {
				case syscall.SIGTERM, syscall.SIGINT:
					log.G(ctx).Infof("Shutting down due to signal %q", s.String())
					cancel()
				case syscall.SIGUSR1:
					buf := make([]byte, 16384)
					n := runtime.Stack(buf, true)
					f, err := ioutil.TempFile("", "systemd-shim")
					if err != nil {
						log.G(ctx).WithError(err).Error("failed to create stack dump file")
					}
					func() {
						defer f.Close()
						f.Write(buf[:n])
						log.G(ctx).Infof("Wrote stack dump to %s", f.Name())
					}()
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	return ctx, cancel
}

func main() {
	var (
		debug      bool
		socket     = defaultAddress
		address    = defaults.DefaultAddress
		namespace  string
		id         string
		publishBin string
		root       string
		bundle     string
		ttrpcAddr  = defaults.DefaultAddress + ".ttrpc"
	)

	flags := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)

	flags.BoolVar(&debug, "debug", false, "enable debug output in the shim")
	flags.StringVar(&address, "address", address, "grpc address back to containerd")
	flags.StringVar(&ttrpcAddr, "ttrpc-address", ttrpcAddr, "ttrpc address back to containerd")
	flags.StringVar(&root, "root", filepath.Join(defaults.DefaultStateDir, "io.containerd.systemd.v1"), "root to store state in")
	flags.StringVar(&socket, "socket", socket, "socket path to serve")

	// Not used, but containerd sets it so we need to have it.
	flags.StringVar(&publishBin, "publish-binary", "", "containerd binary with publish subcommand")
	flags.StringVar(&id, "id", "", "id of the task")
	flags.StringVar(&bundle, "bundle", "", "path to the bundle directory")
	flags.StringVar(&namespace, "namespace", "", "namespace of container")

	var args []string
	if len(os.Args) > 1 {
		args = os.Args[1:]
	}

	if err := flags.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	action := flags.Arg(0)

	ctx, cancel := newCtx()
	defer cancel()
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(logrus.Fields{
		"action": action,
	}))

	errOut := func(err error) {
		if err == nil {
			return
		}

		prefix := path.Join(namespace, id)
		if prefix != "" {
			prefix += ":"
		}

		fmt.Fprintln(os.Stderr, prefix, err)
		os.Exit(3)
	}

	switch action {
	case "install":
		err := install(ctx, root, address, ttrpcAddr, socket, debug)
		errOut(err)
		return
	case "uninstall":
		errOut(uninstall(ctx))
		return
	case "delete":
		if bundle != "" {
			defer func() {
				mount.UnmountAll(filepath.Join(bundle, "rootfs"), unix.MNT_DETACH)
				os.RemoveAll(bundle)
			}()
			svc, err := New(ctx, root, nil)
			if err == nil {
				ctx = namespaces.WithNamespace(ctx, namespace)
				svc.Delete(ctx, &taskapi.DeleteRequest{ID: id})
			} else {
				(&runc.Runc{}).Delete(ctx, id, &runc.DeleteOpts{Force: true})
				svc.Delete(ctx, &taskapi.DeleteRequest{ID: id})
			}
		}
		err := proto.MarshalText(os.Stdout, &taskapi.DeleteResponse{})
		errOut(err)
		return
	case "start":
		_, err := os.Stdout.WriteString("unix:///" + socket)
		errOut(err)
		return
	case "serve":
		publisher, err := shim.NewPublisher(ttrpcAddr)
		errOut(err)

		err = serve(ctx, root, publisher)
		errOut(err)
		return
	default:
		errOut(fmt.Errorf("unknown action: %s", action))
	}
}

func serve(ctx context.Context, root string, publisher events.Publisher) error {
	log.G(ctx).Info("Starting...")

	shm, err := New(ctx, root, publisher)
	if err != nil {
		return err
	}

	svc, err := newService(shm)
	if err != nil {
		return err
	}
	defer svc.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	listeners, err := activation.Listeners()
	if err != nil {
		return err
	}
	for _, l := range listeners {
		ul, ok := l.(*net.UnixListener)
		if !ok {
			panic(fmt.Sprintf("listener type not supported: %T", l))
		}

		log.G(ctx).WithField("addr", ul.Addr()).Info("Serving shim api")
		go func(l net.Listener) {
			svc.Serve(ctx, l)
			cancel()
		}(l)
	}

	go shm.Forward(ctx, publisher)

	<-ctx.Done()
	svc.Close()
	shm.Close()
	return ctx.Err()
}
