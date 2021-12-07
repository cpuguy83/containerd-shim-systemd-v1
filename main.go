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
	"errors"
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
		debug          bool
		socket         = defaultAddress
		address        = defaults.DefaultAddress
		namespace      string
		id             string
		publishBin     string
		root           string
		bundle         string
		ttrpcAddr      = address + ".ttrpc"
		logMode        = defaultLogMode
		noNewNamespace bool
	)

	rootFlags := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)

	rootFlags.StringVar(&address, "address", address, "grpc address back to containerd")

	// Not used, but containerd sets it so we need to have it.
	rootFlags.StringVar(&publishBin, "publish-binary", "", "containerd binary with publish subcommand")
	rootFlags.StringVar(&id, "id", "", "id of the task")
	rootFlags.StringVar(&bundle, "bundle", "", "path to the bundle directory")
	rootFlags.StringVar(&namespace, "namespace", "", "namespace of container")
	rootFlags.BoolVar(&debug, "debug", debug, "enable debug output in the shim")
	rootFlags.StringVar(&ttrpcAddr, "ttrpc-address", ttrpcAddr, "address to containerd ttrpc socket")

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
	flags.BoolVar(&noNewNamespace, "no-new-namespace", noNewNamespace, "mount container rootfs in host namespace")

	traceCfg := TraceFlags(flags)

	doMount := func(ctx context.Context) error {
		cfgData, err := os.ReadFile(flags.Arg(0))
		if err != nil {
			return err
		}

		var cfg taskapi.CreateTaskRequest
		if err := proto.Unmarshal(cfgData, &cfg); err != nil {
			return fmt.Errorf("error unmarshalling task create: %w", err)
		}

		target, err := mountFS(cfg.Rootfs, cfg.Bundle)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Mounted rootfs to", target)
		return err
	}
	commands := map[string]func(context.Context) error{
		"install": func(ctx context.Context) error {
			cfg := installConfig{
				Root:           root,
				Addr:           address,
				TTRPCAddr:      ttrpcAddr,
				Debug:          debug,
				Socket:         socket,
				LogMode:        options.LogMode(options.LogMode_value[strings.ToUpper(logMode)]),
				Trace:          *traceCfg,
				NoNewNamespace: noNewNamespace,
			}
			return install(ctx, cfg)
		},
		"uninstall": uninstall,
		"delete": func(ctx context.Context) error {
			var (
				resp *taskapi.DeleteResponse
			)
			if bundle != "" {
				if namespace == "" {
					namespace = filepath.Base(filepath.Dir(bundle))
				}
				defer func() {
					mount.UnmountAll(filepath.Join(bundle, "rootfs"), unix.MNT_DETACH)
					os.RemoveAll(bundle)
				}()

				svc, err := New(ctx, Config{Root: root})
				if err != nil {
					return err
				}

				ctx = namespaces.WithNamespace(ctx, namespace)
				resp, err = svc.Delete(ctx, &taskapi.DeleteRequest{ID: id})
				if err != nil && !errdefs.IsNotFound(errdefs.FromGRPC(err)) {
					return err
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
			}
			return nil
		},
		"start": func(ctx context.Context) error {
			addr := "unix://" + socket

			if err := shim.WriteAddress("address", addr); err != nil {
				return err
			}
			_, err := os.Stdout.WriteString(addr)
			return err
		},
		"serve": func(ctx context.Context) error {
			log.G(ctx).Infof("Starting with unit name %s", os.Getenv("UNIT_NAME"))
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
				Root:           root,
				Publisher:      publisher,
				LogMode:        options.LogMode(options.LogMode_value[strings.ToUpper(logMode)]),
				NoNewNamespace: noNewNamespace,
			}
			return serve(ctx, opts)
		},
		"mount": func(ctx context.Context) error {
			if flags.NArg() != 1 {
				return errors.New("mount requires exactly one argument")
			}
			return doMount(ctx)
		},
		"unmount": func(ctx context.Context) error {
			return mount.UnmountAll(flags.Arg(0), 0)
		},
		"create": func(ctx context.Context) error {
			if err := doMount(ctx); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, os.Args[2], os.Args[2:])
			return syscall.Exec(os.Args[3], os.Args[3:], nil)
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

	if err := cmd(ctx); err != nil {
		errOut(fmt.Errorf("%s: %w", action, err))
	}
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
