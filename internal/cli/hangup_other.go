//go:build !linux

package cli

import (
	"context"
	"io"
)

// watchHangup watches nothing off Linux (see hangup_linux.go): a hub-side
// cp-out there notices a hung-up laptop at its first write, as before.
func watchHangup(ctx context.Context, _ io.Writer) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
