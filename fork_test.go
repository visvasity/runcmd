// Copyright (c) 2025 Visvasity LLC

package runcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makeCommand(t *testing.T, args ...string) *exec.Cmd {
	compilerPath := filepath.Join(runtime.GOROOT(), "bin/go")
	if _, err := os.Stat(compilerPath); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(wd, "examples/example.go")
	if _, err := os.Stat(srcPath); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(compilerPath, "run", srcPath)
	cmd.Args = append(cmd.Args, args...)
	return cmd
}

func TestForkCommand(t *testing.T) {
	cmd := makeCommand(t, "--run-time", "1s", "--tag=TestForkCommand")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()

	t.Log(findProcesses(t, "tag=TestForkCommand"))
}

func findProcesses(t *testing.T, pattern string) ([]int, error) {
	output, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		if len(output) == 0 {
			return nil, nil
		}
		t.Error(err)
		return nil, err
	}
	fields := strings.Fields(string(output))
	pids := make([]int, len(fields))
	for i, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, err
		}
		pids[i] = pid
	}
	return pids, nil
}

func killProcesses(t *testing.T, pids ...int) error {
	if len(pids) != 0 {
		cmd := exec.Command("kill", "-9")
		for _, pid := range pids {
			cmd.Args = append(cmd.Args, fmt.Sprintf("%d", pid))
		}
		if err := cmd.Run(); err != nil {
			t.Error(err)
			return err
		}
	}
	return nil
}

type ForkTest struct {
	wg sync.WaitGroup
}

func (v *ForkTest) Close() {
	v.wg.Wait()
}

func (v *ForkTest) kill(ctx context.Context, t *testing.T, pattern string) error {
	if len(pattern) == 0 {
		pattern = "/example"
	}
	pids, err := findProcesses(t, pattern)
	if err != nil {
		return err
	}
	if err := killProcesses(t, pids...); err != nil {
		return err
	}
	return nil
}

type ForkTestItem struct {
	Tag string

	CmdArgs []string

	owner *ForkTest

	server *httptest.Server
	ready  atomic.Bool

	ctx    context.Context
	cancel context.CancelCauseFunc

	stdout bytes.Buffer
	stderr bytes.Buffer
}

func (v *ForkTest) startTestCmd(ctx context.Context, t *testing.T, tag string, args ...string) (*ForkTestItem, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	x := &ForkTestItem{
		CmdArgs: args,
		Tag:     tag,
		ctx:     ctx,
		cancel:  cancel,
		owner:   v,
	}
	x.server = httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		x.ready.Store(true)
	}))

	cmd := makeCommand(t, args...)
	cmd.Args = append(cmd.Args, "--run-time=10m")
	cmd.Args = append(cmd.Args, fmt.Sprintf("--tag=%s", tag))
	cmd.Args = append(cmd.Args, fmt.Sprintf("--ready-url=%s/", x.server.URL))
	//cmd.Stdout, cmd.Stderr = &x.stdout, &x.stderr
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Error(err)
		return nil, err
	}
	v.wg.Add(1)
	go func() {
		cmd.Wait()
		t.Logf("tag=%s %v has died (stdout=%s stderr=%s)", tag, args, bytes.TrimSpace(x.stdout.Bytes()), bytes.TrimSpace(x.stderr.Bytes()))
		x.cancel(os.ErrClosed)
		v.wg.Done()
	}()
	return x, nil
}

func (v *ForkTestItem) Close() {
	<-v.ctx.Done()
	v.server.Close()
}

func (v *ForkTestItem) waitForReady(ctx context.Context, t *testing.T) error {
	cmdCtxCh := v.ctx.Done()
	if hasBackgroundFlag(v.CmdArgs) {
		cmdCtxCh = nil
	}

	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-cmdCtxCh:
			return context.Cause(v.ctx)
		case <-time.After(time.Millisecond):
			if v.ready.Load() {
				return nil
			}
		}
	}
}

func (v *ForkTestItem) getPids(ctx context.Context, t *testing.T) ([]int, error) {
	pids, err := findProcesses(t, fmt.Sprintf("tag=%s", v.Tag))
	if err != nil {
		t.Error(err)
		return nil, err
	}
	if len(pids) != 0 {
		return pids, nil
	}
	return nil, nil
}

func (v *ForkTestItem) kill(ctx context.Context, t *testing.T) error {
	return v.owner.kill(ctx, t, fmt.Sprintf("tag=%s", v.Tag))
}

func (v *ForkTestItem) waitToDie(ctx context.Context, t *testing.T) error {
	pids, err := v.getPids(ctx, t)
	if err != nil {
		return err
	}

	cmdCtxCh := v.ctx.Done()
	if hasBackgroundFlag(v.CmdArgs) {
		cmdCtxCh = nil
	}

	for {
		nlive := 0
		select {
		case <-ctx.Done():
			return context.Cause(ctx)

		case <-cmdCtxCh:
			err := context.Cause(v.ctx)
			if !errors.Is(err, os.ErrClosed) {
				return err
			}
			return nil

		case <-time.After(time.Second):
			log.Printf("waiting for %d out of pids %v to die from %v (tag=%s)", nlive, pids, v.CmdArgs, v.Tag)

		case <-time.After(time.Millisecond):
			list, err := findProcesses(t, fmt.Sprintf("tag=%s", v.Tag))
			if err != nil {
				t.Error(err)
				return err
			}
			nlive = 0
			for _, pid := range pids {
				if slices.Contains(list, pid) {
					nlive++
				}
			}
			if nlive == 0 {
				return nil
			}
		}
	}
}

var commands = [][]string{
	{"run"},
	{"run", "--restart"},
	{"run", "--background"},
	{"run", "--restart", "--background"},
	{"run", "--self-monitor"},
	{"run", "--restart", "--self-monitor"},
	{"run", "--background", "--self-monitor"},
	{"run", "--restart", "--background", "--self-monitor"},
}

func TestCommandMatrix(t *testing.T) {
	var ftest ForkTest
	defer ftest.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := ftest.kill(ctx, t, ""); err != nil {
		t.Fatal(err)
	}

	test := func(firstArgs, secondArgs []string) error {
		first, err := ftest.startTestCmd(ctx, t, "TestCommandMatrix/first", firstArgs...)
		if err != nil {
			t.Error(err)
			return err
		}
		defer first.Close()

		if err := first.waitForReady(ctx, t); err != nil {
			t.Error(err)
			return err
		}

		firstPids, err := first.getPids(ctx, t)
		if err != nil {
			t.Error(err)
			return err
		}
		t.Logf("found pids %v for first command %q", firstPids, firstArgs)

		second, err := ftest.startTestCmd(ctx, t, "TestCommandMatrix/second", secondArgs...)
		if err != nil {
			t.Error(err)
			return err
		}
		defer second.Close()

		if hasRestartFlag(secondArgs) {
			// When there is a --restart flag, first command processes are expected to die.
			if err := first.waitToDie(ctx, t); err != nil {
				t.Error(err)
				return err
			}
			if err := second.waitForReady(ctx, t); err != nil {
				t.Error(err)
				return err
			}
			secondPids, err := second.getPids(ctx, t)
			if err != nil {
				t.Error(err)
				return err
			}
			if len(secondPids) == 0 {
				t.Errorf("wanted non-zero number of second command pids")
				return fmt.Errorf("no processes for the second command")
			}
			t.Logf("found pids %v for second command %q", secondPids, secondArgs)
			<-first.ctx.Done()
			if err := second.kill(ctx, t); err != nil {
				t.Error(err)
				return err
			}
			if err := second.waitToDie(ctx, t); err != nil {
				t.Error(err)
				return err
			}
			return nil
		}

		// Second command processes are expected to die cause there is no --restart flag.
		if err := second.waitToDie(ctx, t); err != nil {
			t.Error(err)
			return err
		}
		<-second.ctx.Done()
		if err := first.kill(ctx, t); err != nil {
			t.Error(err)
			return err
		}
		if err := first.waitToDie(ctx, t); err != nil {
			t.Error(err)
			return err
		}
		return nil
	}

	for i := range len(commands) {
		for j := range len(commands) {
			t.Logf("==>> %v vs %v", commands[i], commands[j])
			if err := test(commands[i], commands[j]); err != nil {
				t.Fatal(err)
			}
			ftest.wg.Wait()
		}
	}
}

func hasBackgroundFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-background" || arg == "--background" || arg == "--background=true" {
			return true
		}
	}
	return false
}

func hasRestartFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-restart" || arg == "--restart" || arg == "--restart=true" {
			return true
		}
	}
	return false
}

func hasSelfMonitorFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-self-monitor" || arg == "--self-monitor" || arg == "--self-monitor=true" {
			return true
		}
	}
	return false
}
