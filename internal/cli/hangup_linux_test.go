package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
)

// hangupBackend streams the guest tar, then holds the download exec until
// its context is cancelled, as a long guest tar would: only a cancel ends
// it.
type hangupBackend struct {
	localCopyBackend
	started chan struct{} // closed once the tar is streamed
}

func (b *hangupBackend) Run(ctx context.Context, o backend.ExecOpts, argv ...string) error {
	if err := b.localCopyBackend.Run(ctx, o, argv...); err != nil {
		return err
	}
	close(b.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return fmt.Errorf("the download exec was never cancelled")
	}
}

// The hub side of cp-out stops as soon as the laptop hangs up (stdout's
// reader gone) mid-download, before anything is written: the guest exec is
// cancelled.
func TestCopyOutToTarStopsOnHangup(t *testing.T) {
	home := t.TempDir()
	writeTree(t, home, "source")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	b := &hangupBackend{localCopyBackend: localCopyBackend{home: home}, started: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- copyOutToTar(context.Background(), b, "vm", "source", false, w, io.Discard) }()
	select {
	case <-b.started:
	case err := <-done:
		t.Fatalf("cp-out ended before the download: %v", err)
	}
	r.Close() // the laptop's ssh is gone
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cp-out to a hung-up stdout succeeded")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("cp-out did not notice the hangup")
	}
}

// A stdout whose reader stays does not cancel anything.
func TestWatchHangupQuietWhileRead(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := watchHangup(context.Background(), w)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("cancelled with the reader still open")
	case <-time.After(3 * hangupPollMillis * time.Millisecond):
	}
	r.Close()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not cancelled after the reader closed")
	}
}
