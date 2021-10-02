package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/cpuguy83/systemdshim"
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
		socket     string
		address    string
		namespace  string
		id         string
		publishBin string
		root       string

		stdin  = os.Getenv("STDIN_PATH")
		stdout = os.Getenv("STDOUT_PATH")
		stderr = os.Getenv("STDERR_PATH")
		// isTerm  = os.Getenv("IS_TERMINAL")
	)

	flags := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)

	flags.BoolVar(&debug, "debug", false, "enable debug output in the shim")
	flags.StringVar(&socket, "socket", "", "socket path to serve")
	flags.StringVar(&address, "address", "", "grpc address back to containerd")

	flags.StringVar(&namespace, "namespace", "", "namespace of container")
	flags.StringVar(&root, "root", "", "root to store state in")

	// Not used, but containerd sets it so we need to have it.
	flags.StringVar(&publishBin, "publish-binary", "", "containerd binary with publish subcommand")
	flags.StringVar(&id, "id", "", "id of the task")

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

		log.G(ctx).Errorf("%s -- %s", flags.Args(), os.Args[1:])

		fmt.Fprintln(os.Stderr, namespace+"/"+id+":", err)
		os.Exit(3)
	}

	switch action {
	case "run":
		errOut(run(ctx, stdin, stdout, stderr, append([]string{flags.Args()[1]}, flags.Args()[1:]...)))
		return
	case "delete":
		resp, err := proto.Marshal(&taskapi.DeleteResponse{})
		errOut(err)
		os.Stdout.Write(resp)
		return
	case "start":
		socket, err := start(ctx, systemdshim.StartOpts{
			Address:      address,
			TTRPCAddress: os.Getenv("TTRPC_ADDRESS"),
			Namespace:    namespace,
			Debug:        debug,
		})
		errOut(err)

		_, err = os.Stdout.WriteString(socket)
		errOut(err)
		return
	case "serve":
		publisher, err := shim.NewPublisher(os.Getenv("TTRPC_ADDRESS"))
		errOut(err)

		err = serve(ctx, namespace, socket, root, publisher)
		errOut(err)
		return
	default:
		errOut(fmt.Errorf("unknown action: %s", action))
	}
}

func start(ctx context.Context, opts systemdshim.StartOpts) (string, error) {
	shim, err := systemdshim.New(ctx, opts.Namespace, "", nil)
	if err != nil {
		return "", err
	}
	return shim.StartShim(ctx, opts)
}

func serve(ctx context.Context, ns, address, root string, publisher events.Publisher) error {
	log.G(ctx).Info("Starting...")
	// f, err := fifo.OpenFifoDup2(ctx, "log", unix.O_WRONLY, 0700, int(os.Stderr.Fd()))
	// if err != nil {
	// 	return nil
	// }
	// defer f.Close()
	// logrus.SetOutput(f)

	shm, err := systemdshim.New(ctx, ns, root, publisher)
	if err != nil {
		return err
	}

	svc, err := NewService(shm)
	if err != nil {
		return err
	}
	defer svc.Close()

	if address == "" {
		return errors.New("missing listen address")
	}

	log.G(ctx).WithField("address", address).Info("Listen")
	p := strings.TrimPrefix(address, "unix://")

	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return fmt.Errorf("error ensuring shim socket parent dir exists")
	}

	unix.Unlink(p)
	l, err := net.Listen("unix", p)
	if err != nil {
		return err
	}
	defer l.Close()

	ctx, cancel := context.WithCancel(ctx)
	go func() {
		svc.Serve(ctx, l)
		cancel()
	}()

	go shm.Forward(ctx, publisher)

	<-ctx.Done()
	svc.Close()
	shm.Close()
	return ctx.Err()
}

func run(ctx context.Context, stdin, stdout, stderr string, args []string) error {
	// NOTE: If I don't open theses with O_RDWR then I end up with a SIGPIPE
	var stdinFd int
	if stdin != "" {
		fd, err := unix.Open(stdin, unix.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("error opening stdin fifo: %w", err)
		}
		stdinFd = fd
	}

	var stdoutFd int
	if stdout != "" {
		fd, err := unix.Open(stdout, unix.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("error opening stdout fifo: %w", err)
		}
		stdoutFd = fd
	}

	var stderrFd int
	if stderr != "" {
		fd, err := unix.Open(stderr, unix.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("error opening stderr fifo: %w", err)
		}
		stderrFd = fd
	}

	if stdinFd > 0 {
		if err := unix.Dup2(stdinFd, 0); err != nil {
			return fmt.Errorf("error setting stdin: %w", err)
		}
	}

	if stdoutFd > 0 {
		if err := unix.Dup2(stdoutFd, 1); err != nil {
			return fmt.Errorf("error setting stdout: %w", err)
		}
	}
	if stderrFd > 0 {
		if err := unix.Dup2(stderrFd, 2); err != nil {
			return fmt.Errorf("error setting stderr: %w", err)
		}
	}

	return syscall.Exec(args[0], args[1:], os.Environ())
}
