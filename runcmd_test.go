// Copyright (c) 2025 Visvasity LLC

package runcmd

import (
	"context"
	"errors"
	"flag"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/visvasity/cli"
)

type TestRunCmd struct {
	RunFlags

	runner cli.CmdFunc
}

func (c *TestRunCmd) Command() (string, *flag.FlagSet, cli.CmdFunc) {
	return "run", c.RunFlags.FlagSet(), c.RunFlags.WithRunFunc(c.runner)
}

func TestBasicGoodRun(t *testing.T) {
	ctx := context.Background()
	count := 0
	runner := func(ctx context.Context, args []string) error {
		count++
		return nil
	}
	cmds := []cli.Command{
		&TestRunCmd{runner: runner},
	}
	if err := cli.Run(ctx, cmds, []string{"run"}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("want count=1, got %d", count)
	}
}

func TestBasicBadRun(t *testing.T) {
	ctx := context.Background()
	count := 0
	errBad := errors.New("BAD")
	runner := func(ctx context.Context, args []string) error {
		count++
		return errBad
	}
	cmds := []cli.Command{
		&TestRunCmd{runner: runner},
	}
	if err := cli.Run(ctx, cmds, []string{"run"}); !errors.Is(err, errBad) {
		t.Fatalf("want %v, got %v", errBad, err)
	}
	if count != 1 {
		t.Fatalf("want count=1, got %d", count)
	}
}

func TestSingleServiceInstance(t *testing.T) {
	var wg sync.WaitGroup
	defer wg.Wait()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(os.ErrClosed)

	var count atomic.Int32
	runner := func(ctx context.Context, args []string) error {
		count.Add(1)
		<-ctx.Done()
		return context.Cause(ctx)
	}
	cmds1 := []cli.Command{
		&TestRunCmd{runner: runner},
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := cli.Run(ctx, cmds1, []string{"run"}); err != nil {
			if !errors.Is(err, os.ErrClosed) {
				t.Error(err)
			}
		}
	}()

	// Wait for the go routine to begin running.
	for count.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	cmds2 := []cli.Command{
		&TestRunCmd{runner: runner},
	}
	if err := cli.Run(ctx, cmds2, []string{"run"}); err == nil {
		t.Fatal("wanted non-nil error")
	}
	if count.Load() != 1 {
		t.Fatalf("want count=1, got %d", count.Load())
	}
}

func TestForegroundRestart(t *testing.T) {
	var wg sync.WaitGroup
	defer wg.Wait()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(os.ErrClosed)

	var count atomic.Int32
	runner := func(ctx context.Context, args []string) error {
		count.Add(1)
		<-ctx.Done()
		return context.Cause(ctx)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		cmds1 := []cli.Command{
			&TestRunCmd{runner: runner},
		}
		if err := cli.Run(ctx, cmds1, []string{"run"}); err == nil {
			t.Errorf("want non-nil error for the --restart flag")
		}
	}()

	// Wait for the go routine to begin running.
	for count.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		cmds2 := []cli.Command{
			&TestRunCmd{runner: runner},
		}
		if err := cli.Run(ctx, cmds2, []string{"run", "--restart"}); err != nil {
			if !errors.Is(err, os.ErrClosed) {
				t.Error(err)
			}
		}
	}()

	for count.Load() != 2 {
		time.Sleep(time.Millisecond)
	}
}
