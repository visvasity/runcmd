// Copyright (c) 2025 Visvasity LLC

package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"net/http"
	_ "net/http/pprof"

	"github.com/visvasity/cli"
	"github.com/visvasity/runcmd"
	"github.com/visvasity/sglog"
)

func main() {
	cmds := []cli.Command{
		&RunCmdExample{
			RunFlags: runcmd.RunFlags{
				DataDir: filepath.Join(os.Getenv("HOME"), ".runcmd-example"),
				LogOptions: sglog.Options{
					LogFileHeader: true,
				},
			},
		},
	}
	if err := cli.Run(context.Background(), cmds, os.Args); err != nil {
		log.Fatal(err)
	}
}

// RunCmdExample is a sub-command that uses demonstrates how to use the
// background/self-monitor/restart features implemented by the runcmd package.
type RunCmdExample struct {
	runcmd.RunFlags

	RunTime time.Duration

	FailRandomly bool
}

func (c *RunCmdExample) Purpose() string {
	return "Test program to demo runcmd package"
}

func (c *RunCmdExample) Command() (string, *flag.FlagSet, cli.CmdFunc) {
	fset := c.RunFlags.FlagSet()
	fset.DurationVar(&c.RunTime, "run-time", time.Hour, "When non-zero, run will die after the timeout")
	fset.BoolVar(&c.FailRandomly, "fail-randomly", false, "When true, run is made to fail to start")
	return "run", fset, c.RunFlags.WithRunFunc(c.run)
}

func (c *RunCmdExample) run(ctx context.Context, args []string) error {
	slog.Info("new instance of the runcmd-example program is started")
	runcmd.Report(ctx, nil) // Report successful startup.

	if c.RunTime != 0 {
		tctx, cancel := context.WithTimeoutCause(ctx, c.RunTime, nil)
		defer cancel()

		ctx = tctx
	}

	if c.FailRandomly {
		timeout := time.Duration(10+rand.N(60)) * time.Second
		slog.Info("this program will intentionally panic due to --fail-randomly=true", "timeout", timeout)
		go func() {
			time.Sleep(timeout)
			panic("panic due to --fail-randomly=true")
		}()
	}

	go func() {
		http.ListenAndServe("localhost:6060", nil)
	}()

	for ctx.Err() == nil {
		time.Sleep(time.Second)
		slog.Info("runcmd-example program is running actively")
	}

	return context.Cause(ctx)
}
