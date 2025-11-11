// Copyright (c) 2025 Visvasity LC

// Package runcmd handles the logic for command-line flags --data-dir,
// --background, --restart, --self-monitor, etc. so that, other subcommands can
// reuse this functionality without duplication.
package runcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/visvasity/cli"
	"github.com/visvasity/sglog"
	"github.com/visvasity/unixlock"
)

type RunFlags struct {
	// DataDir (-data-dir) holds the data directory for the service. Defaults to
	// os.TempDir.
	DataDir string

	// LogDir (-logdir) holds the directory for the log files. Defaults to
	// subdirectory named "logs" inside the data directory.
	LogDir string
	// LogName (-logname) holds the name prefix for the log files. Defaults to
	// the base name of the executable.
	LogName string

	// LogToStderr (-logtostderr) when true redirects logs to the standard stderr
	// (including the background processes) and logs are NOT written to the files
	// in the logs directory.
	LogToStderr bool

	// Restart (-restart) when true, sends shutdown request to service instance
	// if it is running.
	Restart bool
	// Background (-background) when true, runs the service in the background as
	// a daemon.
	Background bool
	// SelfMonitor (-self-monitor) when true, makes the service to restart
	// automatically after a crash.
	SelfMonitor bool

	wg sync.WaitGroup

	main cli.CmdFunc

	// reportf is a helper function that sends init status to the monitor/daemon
	// process.
	reportf func(error)
}

// WithRunFunc returns cli.CmdFunc that will first process background, restart,
// self-monitor, etc. runcmd package flags and then delegates control to the
// given user-defined function.
func (v *RunFlags) WithRunFunc(f cli.CmdFunc) cli.CmdFunc {
	v.main = f
	return v.run
}

// FlagSet allocates a new flag.FlagSet object and configures it with the run flags.
func (v *RunFlags) FlagSet() *flag.FlagSet {
	fset := new(flag.FlagSet)
	fset.BoolVar(&v.Restart, "restart", v.Restart, "When true, shutdowns existing service.")
	fset.BoolVar(&v.Background, "background", v.Background, "When true, runs as a background daemon.")
	fset.BoolVar(&v.SelfMonitor, "self-monitor", v.SelfMonitor, "When true, auto restarts background service.")
	fset.StringVar(&v.DataDir, "data-dir", v.DataDir, "Data directory.")
	fset.StringVar(&v.LogDir, "logdir", v.LogDir, "Directory for log files.")
	fset.StringVar(&v.LogName, "logname", v.LogName, "File name prefix for the log files.")
	fset.BoolVar(&v.LogToStderr, "logtostderr", v.LogToStderr, "When true, redirect logs to stderr.")
	return fset
}

// LogDirectory returns the preferred logs directory for the current DataDir value.
func (v *RunFlags) LogDirectory() string {
	if v.DataDir == "" {
		return filepath.Join("/tmp")
	}
	return filepath.Join(v.DataDir, "logs")
}

func (v *RunFlags) run(ctx context.Context, args []string) (status error) {
	if v.main == nil {
		return fmt.Errorf("runcmd.RunFlags are not configured with WithRunFunc")
	}
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	// Setup shutdown signal handlers with signal name in the context cause.
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalCh)

	go func() {
		cancel(fmt.Errorf("received signal %s", <-signalCh))
	}()

	binaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to lookup binary: %w", err)
	}

	if v.DataDir == "" {
		v.DataDir = os.TempDir()
	}
	if _, err := os.Stat(v.DataDir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not stat data dir %q: %w", v.DataDir, err)
		}
		if err := os.MkdirAll(v.DataDir, os.FileMode(0700)); err != nil {
			return fmt.Errorf("could not create data dir %q: %w", v.DataDir, err)
		}
	}

	if v.LogName == "" {
		v.LogName = filepath.Base(binaryPath)
	}

	if v.LogDir == "" {
		v.LogDir = filepath.Join(v.DataDir, "logs")
	}
	if _, err := os.Stat(v.LogDir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not stat logs dir %q: %w", v.LogDir, err)
		}
		if err := os.MkdirAll(v.LogDir, os.FileMode(0700)); err != nil {
			return fmt.Errorf("could not create logs dir %q: %w", v.LogDir, err)
		}
	}

	backgroundSock := filepath.Join(v.DataDir, "daemon.sock")
	backgroundLock := unixlock.New(backgroundSock)
	if quit, status := v.handleBackgroundFlag(ctx, backgroundLock, binaryPath); quit {
		return status
	}
	defer func() { unixlock.Report(ctx, backgroundLock, status) }()

	monitorSock := filepath.Join(v.DataDir, "monitor.sock")
	monitorLock := unixlock.New(monitorSock)
	if quit, status := v.handleSelfMonitorFlag(ctx, backgroundLock, monitorLock, binaryPath); quit {
		return status
	}
	defer func() { unixlock.Report(ctx, monitorLock, status) }()

	serviceSock := filepath.Join(v.DataDir, "service.sock")
	serviceLock := unixlock.New(serviceSock)

	closef, err := serviceLock.TryLock(ctx)
	if err != nil {
		if !v.Restart {
			slog.Error("could not acquire service socket file and restart flag is false", "socket", serviceSock, "err", err)
			return fmt.Errorf("another instance of the service is running (restart flag is false): %w", err)
		}
		slog.Debug("acquiring service socket file with a shutdown message", "socket", serviceSock)
		closef, err = serviceLock.Lock(ctx, true /* force */)
		if err != nil {
			return fmt.Errorf("could not acquire service lock with shutdown message: %w", err)
		}
		slog.Info("acquired service socket file through a shutdown message", "socket", serviceSock)
	}
	defer closef()

	ctx, err = unixlock.WithLock(ctx, serviceLock)
	if err != nil {
		return err
	}

	// Redirect logging if we are NOT running in the foreground.
	if v.Background && !v.LogToStderr {
		opts := &sglog.Options{
			Name:                 v.LogName,
			LogFileHeader:        true,
			LogDirs:              []string{v.LogDir},
			LogFileMaxSize:       100 * 1024 * 1024,
			LogFileReuseDuration: time.Hour,
		}
		backend := sglog.NewBackend(opts)
		defer backend.Close()
		slog.Info("service logs are written to the logs directory", "logname", opts.Name, "logdir", v.LogDir)
		slog.SetDefault(slog.New(backend.Handler()))
	}

	v.reportf = func(status error) {
		// Report success to the monitor process or the foreground process.
		if v.SelfMonitor {
			unixlock.Report(ctx, monitorLock, status)
		} else if v.Background {
			unixlock.Report(ctx, backgroundLock, status)
		}
		v.reportf = nil
	}

	if err := v.main(withRunFlags(ctx, v), args); err != nil {
		if v.reportf != nil {
			v.reportf(err)
		}
		return err
	}
	return nil
}

func (v *RunFlags) handleBackgroundFlag(ctx context.Context, backgroundLock *unixlock.Mutex, binPath string) (quit bool, status error) {
	// If the process is started with -background flag, user wants the initial
	// process to (1) start/restart/self-monitor the service in the background
	// and (2) exit quickly with success or failure of the service startup.

	background := v.Background
	if len(os.Getenv("RUNCMD_DISABLE_BACKGROUND_FLAG")) != 0 {
		background = false
	}
	if !background {
		return false, nil
	}

	closef, err := backgroundLock.Lock(ctx, false /* shutdown */)
	if err != nil {
		return true, fmt.Errorf("could not acquire lock on the daemonize lock file: %w", err)
	}
	defer closef()

	slog.Info("daemonize lock file is acquired/created successfully", "socket", backgroundLock.SocketPath())

	// Disable the -background flag for the forked-child instance and all it's
	// descendants.
	os.Setenv("RUNCMD_DISABLE_BACKGROUND_FLAG", "1")

	file, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return true, fmt.Errorf("failed to open /dev/null: %w", err)
	}
	defer file.Close()

	ctx, cancel := context.WithCancelCause(ctx)
	defer func() { cancel(status) }()

	cmd := exec.Command(binPath, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = file, file, file
	cmd.Dir = "/"
	cmd.Env = []string{
		fmt.Sprintf("PATH=%s", os.Getenv("PATH")),
		fmt.Sprintf("USER=%s", os.Getenv("USER")),
		fmt.Sprintf("HOME=%s", os.Getenv("HOME")),
		fmt.Sprintf("RUNCMD_DISABLE_SELFMONITOR_FLAG=%s", os.Getenv("RUNCMD_DISABLE_SELFMONITOR_FLAG")),
		fmt.Sprintf("RUNCMD_DISABLE_BACKGROUND_FLAG=%s", os.Getenv("RUNCMD_DISABLE_BACKGROUND_FLAG")),
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	if v.LogToStderr {
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		slog.Error("could not start background process", "err", err)
		return true, err
	}
	slog.Info("started new background process", "pid", cmd.Process.Pid)

	v.wg.Add(1)
	go func() {
		childStatus := cmd.Wait()
		slog.Info("background process has died", "pid", cmd.Process.Pid, "status", childStatus)
		cancel(childStatus)
		v.wg.Done()
	}()

	status = unixlock.WaitForReport(ctx, backgroundLock)
	if status == nil {
		slog.Info("background process is initialized successfully", "pid", cmd.Process.Pid)
	} else {
		slog.Info("background process has failed to initialize", "pid", cmd.Process.Pid, "status", status)
	}

	return true, status // End of the foreground process.
}

func (v *RunFlags) handleSelfMonitorFlag(ctx context.Context, backgroundLock, monitorLock *unixlock.Mutex, binPath string) (quit bool, status error) {
	selfMonitor := v.SelfMonitor
	if len(os.Getenv("RUNCMD_DISABLE_SELFMONITOR_FLAG")) != 0 {
		selfMonitor = false
	}

	// If there is another self-monitor instance running, we should not proceed
	// independently if we are or are-not self-monitoring. So a successful return
	// indicates either no-other self-monitor active or self-monitor is active
	// for overselves.

	// When current process has no -self-monitor flag and there is another
	// self-monitor instance running, we should stop the other instance based on
	// the restart flag. Otherwise, we may end up with racing against the other
	// monitor repeatedly fail trying to start a new service and filling up the
	// logs.

	if !selfMonitor && !v.Restart {
		// Ensure that no self-monitored instance is running.
		if closef, err := monitorLock.TryLock(ctx); err == nil {
			closef()
			return false, nil
		}
		if err := monitorLock.CheckAncestor(ctx); err != nil {
			slog.Info("another monitor is active and restart flag is false", "socket", monitorLock.SocketPath(), "err", err)
			return true, os.ErrExist
		}
		return false, nil // my ancestor has the lock
	}

	if !selfMonitor && v.Restart {
		// Check and stop self-monitor if any is already running.
		if closef, err := monitorLock.TryLock(ctx); err == nil {
			closef()
			return false, nil
		}
		if err := monitorLock.CheckAncestor(ctx); err != nil {
			closef, err := monitorLock.Lock(ctx, true /* shutdown */)
			if err != nil {
				slog.Warn("could not shutdown existing monitor", "socket", monitorLock.SocketPath(), "err", err)
				return true, err
			}
			closef()
		}
		return false, nil // my ancestor has the lock
	}

	if selfMonitor && !v.Restart {
		closef, err := monitorLock.TryLock(ctx)
		if err != nil {
			slog.Warn("could not acquire self-monitoring lock and restart flag is false", "err", err)
			return true, err
		}
		defer closef()
	}

	if selfMonitor && v.Restart {
		closef, err := monitorLock.Lock(ctx, true /* shutdown */)
		if err != nil {
			slog.Warn("could not acquire self-monitor lock with shutdown message", "err", err)
			return true, err
		}
		defer closef()
	}

	ctx, err := unixlock.WithLock(ctx, monitorLock)
	if err != nil {
		return true, err
	}

	// Disable -self-monitor flag for the forked children.
	os.Setenv("RUNCMD_DISABLE_SELFMONITOR_FLAG", "1")

	// Redirect logging if we are NOT running in the foreground.
	if v.Background && !v.LogToStderr {
		opts := &sglog.Options{
			Name:                 v.LogName + "-monitor",
			LogFileHeader:        true,
			LogDirs:              []string{v.LogDir},
			LogFileMaxSize:       100 * 1024 * 1024,
			LogFileReuseDuration: time.Hour,
		}
		backend := sglog.NewBackend(opts)
		defer backend.Close()
		slog.Info("self-monitoring logs are written to the logs directory", "logname", opts.Name, "logdir", v.LogDir)
		slog.SetDefault(slog.New(backend.Handler()))
	}

	for i := 0; ctx.Err() == nil; i++ {
		slog.Info("starting a monitored child process", "instance", i)
		status := v.monitorChild(ctx, monitorLock, backgroundLock, binPath, os.Args[1:])
		if status != nil {
			slog.Warn("monitored child process died", "instance", i, "status", status)
		}
		if status == nil {
			slog.Warn("monitored child process quit with success status", "instance", i)
		}

		if i == 0 {
			backgroundLock = nil
			if status != nil {
				return true, status // First child failure is reported back to the user.
			}
		}

		time.Sleep(50*time.Millisecond + time.Duration(rand.Intn(100))*time.Millisecond)
	}
	return true, context.Cause(ctx)
}

// monitorChild forks a child process and waits for the process to die. Returns
// the initialization status of the child process.
//
// Child process is expected to report it's initialization status and still
// continue execution.
//
// If child process dies without reporting the initialization status, then it's
// exit status is used as the initialization status.
//
// Note that child process may die with unsuccessful exit status, but a
// successful initialization status, in which case, this method returns nil.
func (v *RunFlags) monitorChild(ctx context.Context, monitorLock, backgroundLock *unixlock.Mutex, binPath string, args []string) (status error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer func() { cancel(status) }()

	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		if backgroundLock != nil {
			unixlock.Report(ctx, backgroundLock, status)
		}
		slog.Error("child process command has failed (will retry)", "err", err)
		return err
	}
	slog.Info("started new child process", "pid", cmd.Process.Pid)

	v.wg.Add(1)
	go func() {
		childStatus := cmd.Wait()
		slog.Info("child process has died", "pid", cmd.Process.Pid, "status", childStatus)
		cancel(childStatus)
		v.wg.Done()
	}()

	status = unixlock.WaitForReport(ctx, monitorLock)
	slog.Info("received initialization-status report from child process", "pid", cmd.Process.Pid, "status", status)

	if backgroundLock != nil {
		unixlock.Report(ctx, backgroundLock, status)
	}

	<-ctx.Done()

	return status
}
