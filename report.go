// Copyright (c) 2025 Visvasity LLC

package runcmd

import "context"

type runcmdContextKey int

var runcmdContextValue runcmdContextKey

func withRunFlags(ctx context.Context, rflags *RunFlags) context.Context {
	return context.WithValue(ctx, runcmdContextValue, rflags)
}

func fromContext(ctx context.Context) (*RunFlags, bool) {
	v, ok := ctx.Value(runcmdContextValue).(*RunFlags)
	return v, ok
}

// Report function sends success or failure status to the monitor (if
// --self-monitor flag is true) or to the foreground process (if --background
// flag is true) or is a no op.
func Report(ctx context.Context, status error) {
	if r, ok := fromContext(ctx); ok && r.reportf != nil {
		r.reportf(status)
	}
}
