// Copyright (c) 2025 Visvasity LLC

package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	"github.com/visvasity/cli"
	"github.com/visvasity/runcmd"
)

func main() {
	cmds := []cli.Command{
		&ExampleRunCmd{
			RunFlags: runcmd.RunFlags{
				DataDir:     "/tmp/hello",
				LogToStderr: true,
			},
		},
	}
	cli.Run(context.Background(), cmds, os.Args)
}

// ExampleRunCmd is a sub-command that uses demonstrates how to use the
// background/self-monitor/restart features implemented by the runcmd package.
type ExampleRunCmd struct {
	runcmd.RunFlags

	// RunTime determines the amount of time to run.
	RunTime time.Duration

	// Fail makes the subcommand fail to initialize.
	Fail bool

	// FailWithReport when true the subcommand fail with an error reported back
	// to the parent if any; when false subcommand will die silently and parent
	// process if any shall notice (and may be restart) by other means.
	FailWithReport bool

	tag string

	readyURL string
}

func (c *ExampleRunCmd) Purpose() string {
	return "Example program to demo using runcmd package"
}

func (c *ExampleRunCmd) Command() (string, *flag.FlagSet, cli.CmdFunc) {
	fset := c.RunFlags.FlagSet()
	fset.DurationVar(&c.RunTime, "run-time", time.Millisecond, "Amount of time to run")
	fset.BoolVar(&c.Fail, "fail", false, "When true, subcommand is made to fail to start")
	fset.BoolVar(&c.FailWithReport, "fail-with-report", true, "When false, subcommand is made to die silently")
	fset.StringVar(&c.tag, "tag", "", "a tag for tests to identify processes from cmdline")
	fset.StringVar(&c.readyURL, "ready-url", "", "an URL to send startup signal for tests")
	return "run", fset, c.RunFlags.WithRunFunc(c.run)
}

func (c *ExampleRunCmd) run(ctx context.Context, args []string) error {
	if len(c.readyURL) != 0 {
		http.Get(c.readyURL)
	}

	if c.Fail {
		errFail := errors.New("FAILED")
		if c.FailWithReport {
			// Fail, but report failure to the parent.
			runcmd.Report(ctx, errFail)
			return errFail
		}
		// Fail silently without reporting to the parent.
		return errFail
	}
	runcmd.Report(ctx, nil) // Report successful startup.

	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-time.After(c.RunTime):
		return nil
	}
}
