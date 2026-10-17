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

// hangupBackend makes the guest stage, then holds the staging exec until
// its context is cancelled, as a long guest tar would: only a cancel ends
// it.
type hangupBackend struct {
	localDownloadBackend
	stage  string
	staged chan struct{} // closed once the stage exists
}

func (b *hangupBackend) Run(ctx context.Context, o backend.ExecOpts, argv ...string) error {
	if argv[0] == "sh" && b.stage == "" && len(argv) == 7 { // copyOutScript with a stage
		if err := b.localDownloadBackend.Run(ctx, o, argv...); err != nil {
			return err
		}
		b.stage = argv[len(argv)-1]
		close(b.staged)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return fmt.Errorf("the staging exec was never cancelled")
		}
	}
	return b.localDownloadBackend.Run(ctx, o, argv...)
}

// The hub side of cp-out stops as soon as the laptop hangs up (stdout's
// reader gone) mid-download, before anything is written: the guest exec is
// cancelled, CopyOut never runs, and the stage is removed.
func TestCopyOutToTarStopsOnHangup(t *testing.T) {
	home := t.TempDir()
	writeTree(t, home, "source")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	b := &hangupBackend{localDownloadBackend: localDownloadBackend{localCopyBackend: localCopyBackend{home: home}},
		staged: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- copyOutToTar(context.Background(), b, "vm", "source", false, w, io.Discard) }()
	select {
	case <-b.staged:
	case err := <-done:
		t.Fatalf("cp-out ended before staging: %v", err)
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
	if _, err := os.Lstat(b.stage); !os.IsNotExist(err) {
		t.Fatalf("stage %s left behind (%v)", b.stage, err)
	}
	if len(b.stages) != 0 {
		t.Fatalf("CopyOut ran after the hangup: %q", b.stages)
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
