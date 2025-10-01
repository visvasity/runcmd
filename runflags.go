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
	// (including the background processes). Logs are NOT written to the files in
	// the logs directory.
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

	daemonSock := filepath.Join(v.DataDir, "daemon.sock")
	daemonLock := unixlock.New(daemonSock)
	if v.Background {
		defer func() {
			unixlock.Report(ctx, daemonLock, status)
		}()

		// Acquire exclusive ownership on the daemon lock file. Daemon lock file is
		// expected to be held only for short duration, so restart flag is ignored.
		closef, err := daemonLock.TryLock(ctx)
		if err != nil {
			if err := daemonLock.CheckAncestor(ctx); err != nil {
				return fmt.Errorf("could not acquire lock on the daemon lock file: %w", err)
			}
			// Background process continues.
		}
		if closef != nil {
			// Foreground process.
			defer closef()
			slog.Info("daemon socket file is acquired/created successfully", "socket", daemonSock)

			cmd, err := v.daemonize(ctx, binaryPath, os.Args[1:])
			if err != nil {
				return fmt.Errorf("could not start background process: %w", err)
			}
			if err := waitForReport(ctx, daemonLock, cmd); err != nil {
				slog.Error("could not initialize background process", "err", err)
				return fmt.Errorf("background process died with status: %w", err)
			}
			slog.Info("background process is initialized successfully")
			return nil // End of the foreground process.
		}
		// Only the background process continues further.
	}

	monitorSock := filepath.Join(v.DataDir, "monitor.sock")
	monitorLock := unixlock.New(monitorSock)
	if !v.SelfMonitor {
		// Stop monitor process if any.
		if err := v.stopMonitor(ctx, monitorLock); err != nil {
			return err
		}
	}
	if v.SelfMonitor {
		defer func() {
			unixlock.Report(ctx, monitorLock, status)
		}()

		// Acquire exclusive ownership on the monitor lock file.
		closef, err := monitorLock.TryLock(ctx)
		if err != nil {
			if err := monitorLock.CheckAncestor(ctx); err != nil {
				if !v.Restart {
					return fmt.Errorf("another monitor that is not an ancestor is active (restart flag is false): %w", err)
				}
				slog.Info("requesting existing monitor to stop with a shutdown message", "socket", monitorSock)
				closef, err = monitorLock.Lock(ctx, true)
				if err != nil {
					return fmt.Errorf("could not acquire monitor lock with shutdown message: %w", err)
				}
				slog.Info("acquired monitor socket lock through a shutdown message", "socket", monitorSock)
			} else {
				slog.Info("assuming/becoming child cause ancestor process is the active monitor")
			}
		}

		if closef != nil {
			// Monitoring process. Lock file is released only when the shutdown request is received.
			defer closef()

			ctx, err := unixlock.WithLock(ctx, monitorLock)
			if err != nil {
				return err
			}

			for i := 0; ctx.Err() == nil; i++ {
				if err := v.monitorChild(ctx, i, monitorLock, daemonLock, binaryPath, os.Args[1:]); err != nil {
					slog.Warn("monitored child process died", "instance", i, "err", err)
					time.Sleep(50*time.Millisecond + time.Duration(rand.Intn(100))*time.Millisecond)
					continue
				}
				return nil // First child instance could not initialize.
			}
			return context.Cause(ctx) // End of the monitor process.
		}
		// Only the monitored process continues further.
	}

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

	// Initialize the logging backend.
	if !v.LogToStderr {
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
			unixlock.Report(ctx, daemonLock, status)
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

func (v *RunFlags) daemonize(ctx context.Context, binPath string, args []string) (*exec.Cmd, error) {
	file, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open /dev/null: %w", err)
	}
	defer file.Close()

	cmd := &exec.Cmd{
		Path: binPath,
		Args: append([]string{binPath}, args...),
		Dir:  "/",
		Env: []string{
			fmt.Sprintf("PATH=%s", os.Getenv("PATH")),
			fmt.Sprintf("USER=%s", os.Getenv("USER")),
			fmt.Sprintf("HOME=%s", os.Getenv("HOME")),
		},
		Stdin:  file,
		Stdout: file,
		Stderr: file,
		SysProcAttr: &syscall.SysProcAttr{
			Setsid: true,
		},
	}
	if v.LogToStderr {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start process: %w", err)
	}
	return cmd, nil
}

func waitForReport(ctx context.Context, m *unixlock.Mutex, cmd *exec.Cmd) (status error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(status)

	if cmd != nil {
		go func() {
			childStatus := cmd.Wait()
			slog.Info("child process has died", "status", childStatus)
			cancel(childStatus)
		}()
	}

	return unixlock.WaitForReport(ctx, m)
}

func (v *RunFlags) stopMonitor(ctx context.Context, monitorLock *unixlock.Mutex) error {
	// Stop monitor process if any.
	closef, err := monitorLock.TryLock(ctx)
	if err != nil {
		if !v.Restart {
			slog.Info("monitor socket file already exists and restart flag is false", "socket", monitorLock.SocketPath(), "err", err)
			return fmt.Errorf("another monitor that is not an ancestor is active (restart flag is false): %w", err)
		}
		slog.Info("requesting existing monitor to stop with a shutdown message", "socket", monitorLock.SocketPath())
		closef, err = monitorLock.Lock(ctx, true)
		if err != nil {
			return fmt.Errorf("could not acquire monitor lock with shutdown message: %w", err)
		}
		slog.Info("stopped existing monitor with shutdown message", "socket", monitorLock.SocketPath())
	}
	closef()
	return nil
}

func (v *RunFlags) monitorChild(ctx context.Context, instance int, monitorLock, daemonLock *unixlock.Mutex, binPath string, args []string) (status error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(status)

	slog.Info("starting a monitored child process", "instance", instance)
	cmd := exec.CommandContext(ctx, binPath, args...)
	if v.LogToStderr {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		slog.Error("child process command has failed (will retry)", "instance", instance, "err", err)
		return err
	}

	v.wg.Add(1)
	go func() {
		childStatus := cmd.Wait()
		slog.Info("child process has died", "status", childStatus)
		cancel(childStatus)
		v.wg.Done()
	}()

	if instance == 0 {
		status := unixlock.WaitForReport(ctx, monitorLock)
		slog.Info("received status report from monitored child process", "instance", instance, "status", status)

		if v.Background {
			unixlock.Report(ctx, daemonLock, status)
			slog.Info("relayed monitored child's status report to foreground process", "instance", instance)
		}
		if status != nil {
			return nil
		}

		// Send monitor logs to a different file -- only after first instance
		// is successful and background flag is set.
		if v.Background {
			if !v.LogToStderr {
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
		}
	}

	<-ctx.Done()
	return context.Cause(ctx)
}
