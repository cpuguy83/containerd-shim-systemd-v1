package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

const (
	shimCreateHelperName     = "shim-create-helper"
	runcStubHelperName       = "runc-stub"
	managedProcessHelperName = "managed-process"

	runcStubExitCodeAnnotation  = "io.containerd.systemd.test.exit-code"
	runcStubExitDelayAnnotation = "io.containerd.systemd.test.exit-delay"
	runcStubFailpointAnnotation = "io.containerd.systemd.test.failpoint"
	// runcStubRootfsMarkerAnnotation names a file the managed process must find
	// under <bundle>/rootfs, proving the container's rootfs was mounted (in either
	// the host namespace or the unit's private mount namespace).
	runcStubRootfsMarkerAnnotation = "io.containerd.systemd.test.rootfs-marker"

	runcStubExitCodeEnv         = "RUNC_STUB_EXIT_CODE"
	runcStubExitDelayEnv        = "RUNC_STUB_EXIT_DELAY"
	runcStubFailpointEnv        = "RUNC_STUB_FAILPOINT"
	runcStubExitBeforeDetachEnv = "RUNC_STUB_EXIT_BEFORE_DETACH"
	runcStubWaitForReleaseEnv   = "RUNC_STUB_WAIT_FOR_RELEASE"

	runcStubKillAllMarker = "kill-all"
	// runcStubCheckpointImage is a file the test writes into a checkpoint dir;
	// `runc restore` must be handed that dir via --image-path, proving the shim
	// took the restore path with the right checkpoint rather than a plain create.
	runcStubCheckpointImage = "checkpoint.img"

	runcStubCreateFailure = 42
	runcStubStartFailure  = 43
	runcStubExecFailure   = 45
	runcStubRootfsMissing = 66
)

type runcStubConfig struct {
	ExitCode         int           `json:"exitCode"`
	ExitDelay        time.Duration `json:"exitDelay,omitempty"`
	Failpoint        string        `json:"failpoint,omitempty"`
	StartImmediately bool          `json:"startImmediately,omitempty"`
	WaitForReparent  bool          `json:"waitForReparent,omitempty"`
	ExitBeforeDetach bool          `json:"exitBeforeDetach,omitempty"`
	WaitForRelease   bool          `json:"waitForRelease,omitempty"`
	ParentPID        int           `json:"parentPid,omitempty"`
	RootfsCheck      string        `json:"rootfsCheck,omitempty"`
}

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case shimCreateHelperName:
		if err := runShimCreateHelper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case runcStubHelperName:
		os.Exit(runRuncStub())
	case managedProcessHelperName:
		os.Exit(runManagedProcess())
	default:
		os.Exit(m.Run())
	}
}

func runShimCreateHelper() error {
	// The no-new-namespace rootfs path drives ExecStartPre/ExecStopPost, which
	// re-exec the shim as `<exe> mount <cfg>` and `<exe> unmount <rootfs>`.
	if len(os.Args) >= 3 {
		switch os.Args[1] {
		case "mount":
			return mountRootfs(os.Args[2])
		case "unmount":
			return mount.UnmountAll(os.Args[2], 0)
		}
	}

	bundle, mounts, tty, cmd, err := parseShimCreateArgs(os.Args[1:])
	if err != nil {
		return err
	}
	// The PrivateMounts path mounts the rootfs in the create re-exec before runc.
	if mounts != "" {
		if err := mountRootfs(mounts); err != nil {
			return err
		}
	}
	return createCmd(context.Background(), bundle, cmd, tty, mounts != "")
}

// parseShimCreateArgs mirrors main()'s two-stage parse: global flags up to the
// subcommand, then the subcommand's own flags. A single pass would stop at
// "create" and leave --mounts= to be exec'd as the container command, which is
// exactly the shape the PrivateMounts path produces.
func parseShimCreateArgs(argv []string) (bundle, mounts string, tty bool, cmd []string, err error) {
	rootFlags := flag.NewFlagSet(shimCreateHelperName, flag.ContinueOnError)
	rootFlags.Bool("debug", false, "")
	rootBundle := rootFlags.String("bundle", "", "")
	if err := rootFlags.Parse(argv); err != nil {
		return "", "", false, nil, err
	}
	if rootFlags.NArg() < 1 || rootFlags.Arg(0) != "create" {
		return "", "", false, nil, fmt.Errorf("invalid shim helper arguments: %q", argv)
	}

	subFlags := flag.NewFlagSet(shimCreateHelperName+" create", flag.ContinueOnError)
	subTTY := subFlags.Bool("tty", false, "")
	subMounts := subFlags.String("mounts", "", "")
	if err := subFlags.Parse(rootFlags.Args()[1:]); err != nil {
		return "", "", false, nil, err
	}
	if subFlags.NArg() < 1 {
		return "", "", false, nil, fmt.Errorf("invalid shim helper arguments: %q", argv)
	}
	return *rootBundle, *subMounts, *subTTY, subFlags.Args(), nil
}

func runRuncStub() int {
	if len(os.Args) == 2 && os.Args[1] == "features" {
		fmt.Fprint(os.Stdout, `{"ociVersionMin":"1.0.0","ociVersionMax":"1.3.0","mountOptions":["bind","rro"]}`)
		return 0
	}

	root, command, args, err := parseRuncStubInvocation(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}

	var code int
	switch command {
	case "create":
		code, err = runRuncStubCreate(root, args)
	case "restore":
		code, err = runRuncStubRestore(root, args)
	case "exec":
		code, err = runRuncStubExec(args)
	case "start":
		code, err = runRuncStubStart(root, args)
	case "delete":
		code, err = runRuncStubDelete(root, args)
	case "kill":
		code, err = runRuncStubKill(root, args)
	case "state":
		code, err = runRuncStubState(args)
	default:
		err = fmt.Errorf("runc stub does not implement %q", command)
		code = 125
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	return code
}

func runRuncStubKill(root string, args []string) (int, error) {
	if len(args) != 3 || args[0] != "--all" || args[2] != strconv.Itoa(int(syscall.SIGKILL)) {
		return 125, fmt.Errorf("invalid runc kill arguments: %q", args)
	}
	return 0, os.WriteFile(filepath.Join(root, args[1], runcStubKillAllMarker), nil, 0600)
}

func runRuncStubState(args []string) (int, error) {
	if _, err := os.Stat("config.json"); err != nil {
		return 125, fmt.Errorf("read bundle config: %w", err)
	}
	id, err := commandID(args)
	if err != nil {
		return 125, err
	}
	bundle, err := os.Getwd()
	if err != nil {
		return 125, err
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
		"id":     id,
		"pid":    42,
		"status": "running",
		"bundle": bundle,
	}); err != nil {
		return 125, err
	}
	return 0, nil
}

func parseRuncStubInvocation(args []string) (root, command string, commandArgs []string, retErr error) {
	for i := 0; i < len(args); {
		arg := args[i]
		switch {
		case arg == "--root":
			if i+1 == len(args) {
				return "", "", nil, fmt.Errorf("--root requires a value")
			}
			root = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--root="):
			root = strings.TrimPrefix(arg, "--root=")
			i++
		case arg == "--log" || arg == "--log-format" || arg == "--criu":
			if i+1 == len(args) {
				return "", "", nil, fmt.Errorf("%s requires a value", arg)
			}
			i += 2
		case strings.HasPrefix(arg, "-"):
			i++
		default:
			if root == "" {
				return "", "", nil, fmt.Errorf("runc stub invocation has no root: %q", args)
			}
			return root, arg, args[i+1:], nil
		}
	}
	return "", "", nil, fmt.Errorf("runc stub invocation has no command: %q", args)
}

func runRuncStubCreate(root string, args []string) (int, error) {
	bundle, err := commandOption(args, "--bundle")
	if err != nil {
		return 125, err
	}
	pidFile, err := commandOption(args, "--pid-file")
	if err != nil {
		return 125, err
	}
	id, err := commandID(args)
	if err != nil {
		return 125, err
	}

	cfg, err := readRuncStubBundleConfig(bundle)
	if err != nil {
		return 125, err
	}
	if cfg.Failpoint == "create" {
		return runcStubCreateFailure, fmt.Errorf("runc create failpoint")
	}

	stateDir := filepath.Join(root, id)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return 125, err
	}
	return startRuncStubProcess(stateDir, pidFile, cfg)
}

// runRuncStubRestore models `runc restore`: it starts the restored container
// immediately (there is no separate start) and reports its pid. restore carries
// no --pid-file flag, so the pid path comes from the PIDFILE the unit exports.
func runRuncStubRestore(root string, args []string) (int, error) {
	bundle, err := commandOption(args, "--bundle")
	if err != nil {
		return 125, err
	}
	imagePath, err := commandOption(args, "--image-path")
	if err != nil {
		return 125, err
	}
	// restore must be handed the checkpoint the task was created with.
	if _, err := os.Stat(filepath.Join(imagePath, runcStubCheckpointImage)); err != nil {
		return 125, fmt.Errorf("restore image-path %q has no checkpoint: %w", imagePath, err)
	}
	id, err := commandID(args)
	if err != nil {
		return 125, err
	}
	pidFile := os.Getenv("PIDFILE")
	if pidFile == "" {
		return 125, fmt.Errorf("runc restore stub: no PIDFILE in environment")
	}
	cfg, err := readRuncStubBundleConfig(bundle)
	if err != nil {
		return 125, err
	}
	cfg.StartImmediately = true
	stateDir := filepath.Join(root, id)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return 125, err
	}
	return startRuncStubProcess(stateDir, pidFile, cfg)
}

func startRuncStubProcess(stateDir, pidFile string, cfg runcStubConfig) (int, error) {
	cfg.ParentPID = os.Getpid()
	if err := writeRuncStubConfig(stateDir, cfg); err != nil {
		return 125, err
	}
	helper := filepath.Join(filepath.Dir(os.Args[0]), managedProcessHelperName)
	cmd := exec.Command(helper, stateDir)
	if err := cmd.Start(); err != nil {
		return 125, err
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(filepath.Join(stateDir, "pid"), []byte(strconv.Itoa(pid)), 0600); err != nil {
		_ = cmd.Process.Kill()
		return 125, err
	}
	if cfg.ExitBeforeDetach {
		if err := waitForRuncStubProcessExit(pid, 5*time.Second); err != nil {
			_ = cmd.Process.Kill()
			return 125, err
		}
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0600); err != nil {
		_ = cmd.Process.Kill()
		return 125, err
	}
	if err := cmd.Process.Release(); err != nil {
		return 125, err
	}
	return 0, nil
}

func runRuncStubExec(args []string) (int, error) {
	processFile, err := commandOption(args, "--process")
	if err != nil {
		return 125, err
	}
	pidFile, err := commandOption(args, "--pid-file")
	if err != nil {
		return 125, err
	}
	if _, err := commandID(args); err != nil {
		return 125, err
	}

	cfg, err := readRuncStubProcessConfig(processFile)
	if err != nil {
		return 125, err
	}
	if cfg.Failpoint == "exec" {
		return runcStubExecFailure, fmt.Errorf("runc exec failpoint")
	}
	cfg.StartImmediately = true
	cfg.WaitForReparent = !cfg.ExitBeforeDetach
	return startRuncStubProcess(filepath.Dir(pidFile), pidFile, cfg)
}

func runRuncStubStart(root string, args []string) (int, error) {
	id, err := commandID(args)
	if err != nil {
		return 125, err
	}
	stateDir := filepath.Join(root, id)
	cfg, err := readRuncStubConfig(stateDir)
	if err != nil {
		return 125, err
	}
	if cfg.Failpoint == "start" {
		return runcStubStartFailure, fmt.Errorf("runc start failpoint")
	}
	if err := os.WriteFile(filepath.Join(stateDir, "started"), nil, 0600); err != nil {
		return 125, err
	}
	return 0, nil
}

func runRuncStubDelete(root string, args []string) (int, error) {
	id, err := commandID(args)
	if err != nil {
		return 125, err
	}
	stateDir := filepath.Join(root, id)
	// Real runc removes the container's state only on a successful delete, and
	// preserves it on failure. Mirror that: a reused container id then starts from
	// a clean state directory instead of inheriting the previous run's "started"
	// marker, while a failed delete keeps state so lifecycle tests can observe it.
	pidData, err := os.ReadFile(filepath.Join(stateDir, "pid"))
	if err != nil {
		if os.IsNotExist(err) {
			os.RemoveAll(stateDir)
			return 0, nil
		}
		return 125, err
	}
	pid, err := strconv.Atoi(string(pidData))
	if err != nil {
		return 125, err
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		if os.IsNotExist(err) {
			os.RemoveAll(stateDir)
			return 0, nil
		}
		return 125, err
	}
	command := string(cmdline)
	if end := strings.IndexByte(command, 0); end >= 0 {
		command = command[:end]
	}
	if filepath.Base(command) != managedProcessHelperName {
		return 125, fmt.Errorf("refusing to signal unexpected process %d (%q)", pid, command)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return 125, err
	}
	os.RemoveAll(stateDir)
	return 0, nil
}

func runManagedProcess() int {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "managed process requires a state directory: %q\n", os.Args[1:])
		return 125
	}
	stateDir := os.Args[1]
	cfg, err := readRuncStubConfig(stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}

	// If the container declares a rootfs marker, the shim must have mounted the
	// rootfs (host-namespace ExecStartPre or private-namespace --mounts) before
	// the container runs.
	if cfg.RootfsCheck != "" {
		if _, err := os.Stat(cfg.RootfsCheck); err != nil {
			fmt.Fprintln(os.Stderr, "rootfs marker missing:", err)
			return runcStubRootfsMissing
		}
	}

	if !cfg.StartImmediately {
		started := filepath.Join(stateDir, "started")
		for {
			if _, err := os.Stat(started); err == nil {
				break
			} else if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, err)
				return 125
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if cfg.WaitForReparent {
		for os.Getppid() == cfg.ParentPID {
			time.Sleep(time.Millisecond)
		}
	}
	if cfg.WaitForRelease {
		if err := waitForRuncStubFile(filepath.Join(stateDir, "release"), 10*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 125
		}
	}
	time.Sleep(cfg.ExitDelay)
	return cfg.ExitCode
}

func readRuncStubBundleConfig(bundle string) (runcStubConfig, error) {
	data, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return runcStubConfig{}, err
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return runcStubConfig{}, err
	}

	cfg := runcStubConfig{Failpoint: spec.Annotations[runcStubFailpointAnnotation]}
	if err := setRuncStubExitCode(&cfg, spec.Annotations[runcStubExitCodeAnnotation]); err != nil {
		return runcStubConfig{}, err
	}
	if err := setRuncStubExitDelay(&cfg, spec.Annotations[runcStubExitDelayAnnotation]); err != nil {
		return runcStubConfig{}, err
	}
	if marker := spec.Annotations[runcStubRootfsMarkerAnnotation]; marker != "" {
		cfg.RootfsCheck = filepath.Join(bundle, "rootfs", marker)
	}
	return cfg, nil
}

func readRuncStubProcessConfig(processFile string) (runcStubConfig, error) {
	data, err := os.ReadFile(processFile)
	if err != nil {
		return runcStubConfig{}, err
	}
	var process specs.Process
	if err := json.Unmarshal(data, &process); err != nil {
		return runcStubConfig{}, err
	}

	var cfg runcStubConfig
	for _, entry := range process.Env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case runcStubExitCodeEnv:
			if err := setRuncStubExitCode(&cfg, value); err != nil {
				return runcStubConfig{}, err
			}
		case runcStubExitDelayEnv:
			if err := setRuncStubExitDelay(&cfg, value); err != nil {
				return runcStubConfig{}, err
			}
		case runcStubFailpointEnv:
			cfg.Failpoint = value
		case runcStubExitBeforeDetachEnv:
			exitBeforeDetach, err := strconv.ParseBool(value)
			if err != nil {
				return runcStubConfig{}, fmt.Errorf("invalid exit-before-detach value %q: %w", value, err)
			}
			cfg.ExitBeforeDetach = exitBeforeDetach
		case runcStubWaitForReleaseEnv:
			waitForRelease, err := strconv.ParseBool(value)
			if err != nil {
				return runcStubConfig{}, fmt.Errorf("invalid wait-for-release value %q: %w", value, err)
			}
			cfg.WaitForRelease = waitForRelease
		}
	}
	return cfg, nil
}

func waitForRuncStubFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

// waitForRuncStubProcessExit blocks until pid is reapable, without reaping it --
// the exited workload is left for whoever is meant to collect its status.
//
// It asks waitid rather than polling /proc/<pid>/stat for state Z, because those
// are different questions: the child is this binary re-executed and so is
// multi-threaded, and its main thread is Z while the rest of the thread group is
// still exiting, which is a window where wait4 will not yet reap it.
func waitForRuncStubProcessExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var info unix.Siginfo
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); err != nil {
			return err
		}
		// With WNOHANG a child that is not yet reapable leaves the siginfo
		// zeroed rather than reporting an error.
		if info.Signo != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for process %d to exit", pid)
		}
		time.Sleep(time.Millisecond)
	}
}

func setRuncStubExitCode(cfg *runcStubConfig, value string) error {
	if value == "" {
		return nil
	}
	code, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("invalid exit code %q: %w", value, err)
	}
	if code < 0 || code > 255 {
		return fmt.Errorf("exit code %d is out of range", code)
	}
	cfg.ExitCode = code
	return nil
}

func setRuncStubExitDelay(cfg *runcStubConfig, value string) error {
	if value == "" {
		return nil
	}
	delay, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid exit delay %q: %w", value, err)
	}
	cfg.ExitDelay = delay
	return nil
}

func writeRuncStubConfig(stateDir string, cfg runcStubConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, "config.json"), data, 0600)
}

func readRuncStubConfig(stateDir string) (runcStubConfig, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, "config.json"))
	if err != nil {
		return runcStubConfig{}, err
	}
	var cfg runcStubConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return runcStubConfig{}, err
	}
	return cfg, nil
}

func commandOption(args []string, name string) (string, error) {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1], nil
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"="), nil
		}
	}
	return "", fmt.Errorf("runc stub command has no %s option: %q", name, args)
}

func commandID(args []string) (string, error) {
	if len(args) == 0 || strings.HasPrefix(args[len(args)-1], "-") {
		return "", fmt.Errorf("runc stub command has no container ID: %q", args)
	}
	return args[len(args)-1], nil
}
