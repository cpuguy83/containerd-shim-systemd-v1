package main

/*
#cgo CFLAGS: -Wall
extern void handle_pty();
void __attribute__((constructor)) init(void) {
	handle_pty();
}
*/
import "C"

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskapi "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/go-runc"
	"github.com/coreos/go-systemd/v22/activation"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
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

var (
	defaultLogMode = strings.ToLower(options.LogMode_name[int32(options.LogMode_STDIO)])
)

func main() {
	if os.Getenv(ttyHandshakeEnv) == "1" {
		if err := ttyHandshake(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

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
		logMode    = defaultLogMode
	)

	rootFlags := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)

	rootFlags.StringVar(&address, "address", address, "grpc address back to containerd")

	// Not used, but containerd sets it so we need to have it.
	rootFlags.StringVar(&publishBin, "publish-binary", "", "containerd binary with publish subcommand")
	rootFlags.StringVar(&id, "id", "", "id of the task")
	rootFlags.StringVar(&bundle, "bundle", "", "path to the bundle directory")
	rootFlags.StringVar(&namespace, "namespace", "", "namespace of container")
	rootFlags.BoolVar(&debug, "debug", debug, "enable debug output in the shim")

	if err := rootFlags.Parse(os.Args[1:]); err != nil {
		fmt.Println(os.Args)
		rootFlags.Usage()
		os.Exit(1)
	}

	if rootFlags.NArg() == 0 {
		fmt.Println(os.Args)
		rootFlags.Usage()
		os.Exit(1)
	}

	flags := flag.NewFlagSet(rootFlags.Name()+" "+rootFlags.Arg(0), flag.ContinueOnError)

	traceCfg := TraceFlags(flags)

	commands := map[string]func(context.Context) error{
		"install": func(ctx context.Context) error {
			cfg := installConfig{
				Root:      root,
				Addr:      address,
				TTRPCAddr: ttrpcAddr,
				Debug:     debug,
				Socket:    socket,
				LogMode:   options.LogMode(options.LogMode_value[strings.ToUpper(logMode)]),
				Trace:     *traceCfg,
			}
			return install(ctx, cfg)
		},
		"uninstall": uninstall,
		"delete": func(ctx context.Context) error {
			var (
				resp *taskapi.DeleteResponse
				err  error
			)
			if bundle != "" {
				defer func() {
					mount.UnmountAll(filepath.Join(bundle, "rootfs"), unix.MNT_DETACH)
					os.RemoveAll(bundle)
				}()

				svc, err := New(ctx, Config{Root: root})
				if err == nil {
					ctx = namespaces.WithNamespace(ctx, namespace)
					resp, err = svc.Delete(ctx, &taskapi.DeleteRequest{ID: id})
				} else {
					(&runc.Runc{}).Delete(ctx, id, &runc.DeleteOpts{Force: true})
					resp, err = svc.Delete(ctx, &taskapi.DeleteRequest{ID: id})
				}
				if err != nil && !errdefs.IsNotFound(errdefs.FromGRPC(err)) {
					return err
				}
			}

			if resp == nil {
				resp = &taskapi.DeleteResponse{}
			}
			data, err := proto.Marshal(resp)
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(data)
			return err
		},
		"start": func(ctx context.Context) error {
			_, err := os.Stdout.WriteString("unix:///" + socket)
			return err
		},
		"serve": func(ctx context.Context) error {
			done, err := ConfigureTracing(ctx, traceCfg)
			if err != nil {
				return err
			}
			defer done(ctx)

			publisher, err := shim.NewPublisher(ttrpcAddr)
			if err != nil {
				return err
			}

			opts := Config{
				Root:      root,
				Publisher: publisher,
				LogMode:   options.LogMode(options.LogMode_value[strings.ToUpper(logMode)]),
			}
			return serve(ctx, opts)
		},
		"create": func(ctx context.Context) (retErr error) {
			runtime.LockOSThread()

			data, err := os.ReadFile(os.Getenv("CREATE_TASK_CONFIG"))
			if err != nil {
				return fmt.Errorf("error reading task config stream")
			}

			var create taskapi.CreateTaskRequest
			if err := proto.Unmarshal(data, &create); err != nil {
				return fmt.Errorf("error unmarshaling task config data")
			}
			if len(create.Rootfs) > 0 {
				// This is pretty much the whole reason we have this special subcommand.
				// We want to prevent this mount from escaping to the host mount namespace.
				// First, create a new mount ns, then we remount the host rootfs with our own propagation.
				if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
					return err
				}

				// Note: Do not use private mount propagation here, otherwise we'll prevent unmounts for stuff we don't care about on the host.
				if err := unix.Mount("", "/", "", unix.MS_SLAVE|unix.MS_REC, ""); err != nil {
					return err
				}

				var mounts []mount.Mount
				for _, m := range create.Rootfs {
					mounts = append(mounts, mount.Mount{
						Type:    m.Type,
						Source:  m.Source,
						Options: m.Options,
					})
				}

				rootfs := filepath.Join(create.Bundle, "rootfs")
				if err := os.Mkdir(rootfs, 0700); err != nil && !os.IsExist(err) {
					return fmt.Errorf("error creating rootfs dir: %w", err)
				}
				if err := mount.All(mounts, rootfs); err != nil {
					return fmt.Errorf("error mounting rootfs: %w", err)
				}
				defer func() {
					if retErr != nil {
						mount.UnmountAll(rootfs, 0)
					}
				}()
			}

			return syscall.Exec(os.Args[2], os.Args[2:], nil)
		},
	}

	if _, ok := commands[rootFlags.Arg(0)]; !ok {
		rootFlags.Output().Write([]byte("unrecognized command: " + rootFlags.Arg(0) + "\n"))
		rootFlags.Usage()
		cmds := make([]string, 0, len(commands))
		for name := range commands {
			cmds = append(cmds, name)
		}
		sort.Strings(cmds)
		for _, name := range cmds {
			rootFlags.Output().Write([]byte(fmt.Sprintf("\t%s\n", name)))
		}
		os.Exit(1)
	}

	flags.BoolVar(&debug, "debug", debug, "enable debug output in the shim")
	flags.StringVar(&ttrpcAddr, "ttrpc-address", ttrpcAddr, "ttrpc address back to containerd")
	flags.StringVar(&root, "root", filepath.Join(defaults.DefaultStateDir, shimName), "root to store state in")
	flags.StringVar(&socket, "socket", socket, "socket path to serve")

	flags.StringVar(&logMode, "log-mode", logMode, "sets the default log mode for containers")

	if len(os.Args) < 2 {
		flags.Usage()
		os.Exit(1)
	}

	if err := flags.Parse(rootFlags.Args()[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	if logMode == "" {
		logMode = defaultLogMode
	}

	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	action := rootFlags.Arg(0)

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

	cmd, ok := commands[action]
	if !ok {
		errOut(fmt.Errorf("unknown command %v", action))
	}

	errOut(cmd(ctx))
}

func serve(ctx context.Context, cfg Config) error {
	log.G(ctx).Info("Starting...")

	mux := http.NewServeMux()
	mux.HandleFunc("/profile", pprof.Profile)
	go func() {
		if err := http.ListenAndServe("127.0.0.1:8089", mux); err != nil {
			log.G(ctx).WithError(err).Fatal("ListenAndServe")
		}
	}()

	shm, err := New(ctx, cfg)
	if err != nil {
		return err
	}

	svc, err := newService(shm)
	if err != nil {
		return err
	}
	defer svc.Close()

	if err := shm.watchUnits(ctx); err != nil {
		return err
	}

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

	go shm.Forward(ctx, cfg.Publisher)

	<-ctx.Done()
	svc.Close()
	shm.Close()
	return ctx.Err()
}
