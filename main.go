package main

/*
#cgo CFLAGS: -Wall
extern void pty_main();
void __attribute__((constructor)) init(void) {
	pty_main();
}
*/
import "C"

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/atomicfile"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/coreos/go-systemd/v22/activation"
	"github.com/cpuguy83/containerd-shim-systemd-v1/options"
	"github.com/pelletier/go-toml"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
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
					f, err := ioutil.TempFile("", "containerd-shim-systemd-v1-goroutines")
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
		info           bool

		// create cmd
		mountCfg string
		tty      bool
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
	rootFlags.BoolVar(&info, "info", false, "print runtime information and exit")

	if err := rootFlags.Parse(os.Args[1:]); err != nil {
		fmt.Println(os.Args)
		rootFlags.Usage()
		os.Exit(1)
	}

	if info {
		if err := writeRuntimeInfo(context.Background(), os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if rootFlags.NArg() == 0 {
		fmt.Println(os.Args)
		rootFlags.Usage()
		os.Exit(1)
	}

	flags := flag.NewFlagSet(rootFlags.Name()+" "+rootFlags.Arg(0), flag.ContinueOnError)
	flags.BoolVar(&noNewNamespace, "no-new-namespace", noNewNamespace, "mount container rootfs in host namespace")

	traceCfg := TraceFlags(flags)

	doMount := func(ctx context.Context, p string) error {
		cfgData, err := os.ReadFile(p)
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

	containerdConfigPath := filepath.Join(defaults.DefaultConfigDir, "config.toml")
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
				if err != nil && !errdefs.IsNotFound(errgrpc.ToNative(err)) {
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
			return writeBootstrapResponse(os.Stdin, os.Stdout, socket, id, namespace)
		},
		"serve": func(ctx context.Context) error {
			// read the containerd config so we can match log formats defined there.
			var containerdConfig srvConfig
			if f, err := toml.LoadFile(containerdConfigPath); err != nil {
				logrus.WithError(err).Error("Failed to load containerd config")
			} else {
				if err := f.Unmarshal(&containerdConfig); err != nil {
					logrus.WithError(err).Error("Failed to unmarshal conntainerd config")
				}
			}

			setupLogFormat(ctx, containerdConfig)

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
			return doMount(ctx, flag.Arg(0))
		},
		"unmount": func(ctx context.Context) error {
			return mount.UnmountAll(flags.Arg(0), 0)
		},
		"create": func(ctx context.Context) error {
			ctx = log.WithLogger(ctx, log.G(ctx).WithField("unit", os.Getenv("UNIT_NAME")))
			ctx = WithShimLog(ctx, OpenShimLog(ctx, bundle))

			if mountCfg != "" {
				if err := doMount(ctx, mountCfg); err != nil {
					return err
				}
			}
			return createCmd(ctx, bundle, flags.Args(), tty, mountCfg != "")
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

	flags.StringVar(&mountCfg, "mounts", mountCfg, "mount config for container")
	flags.BoolVar(&tty, "tty", tty, "stdio is tty")

	flags.StringVar(&containerdConfigPath, "containerd-config", containerdConfigPath, "path to containerd config")

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

func writeBootstrapResponse(in io.Reader, out io.Writer, socket, id, namespace string) error {
	input, err := io.ReadAll(io.LimitReader(in, 10<<20))
	if err != nil {
		return fmt.Errorf("read bootstrap parameters: %w", err)
	}

	var params bootapi.BootstrapParams
	if len(input) == 0 ||
		proto.Unmarshal(input, &params) != nil ||
		params.GetInstanceID() != id ||
		params.GetNamespace() != namespace {
		address := "unix://" + socket
		if err := writeAddressFile("address", address); err != nil {
			return fmt.Errorf("write legacy bootstrap address: %w", err)
		}
		if _, err := io.WriteString(out, address); err != nil {
			return fmt.Errorf("write legacy bootstrap response: %w", err)
		}
		return nil
	}

	result, err := proto.Marshal(&bootapi.BootstrapResult{
		Version:  3,
		Address:  "unix://" + socket,
		Protocol: "ttrpc",
	})
	if err != nil {
		return fmt.Errorf("marshal bootstrap result: %w", err)
	}
	if _, err := out.Write(result); err != nil {
		return fmt.Errorf("write bootstrap result: %w", err)
	}
	return nil
}

func writeAddressFile(path, address string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	file, err := atomicfile.New(path, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, address); err != nil {
		file.Cancel()
		return err
	}
	return file.Close()
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// React to systemd unit state changes over D-Bus. This is the shim's sole
	// monitor of unit state; it resyncs tracked units on reconnect to recover
	// anything missed while the connection was down.
	shm.watchEvents(ctx)

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
