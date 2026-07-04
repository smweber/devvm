package auth

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/backend"
)

func TestTools(t *testing.T) {
	all, err := Tools("all")
	if err != nil {
		t.Fatalf("Tools(all): %v", err)
	}
	if !reflect.DeepEqual(all, Choices()) {
		t.Fatalf("Tools(all) = %v, want %v", all, Choices())
	}
	one, err := Tools("codex")
	if err != nil || !reflect.DeepEqual(one, []string{"codex"}) {
		t.Fatalf("Tools(codex) = %v, %v", one, err)
	}
	if _, err := Tools("bogus"); err == nil {
		t.Fatal("Tools(bogus) should error")
	}
}

func TestToolBinary(t *testing.T) {
	if got := toolBinary("github"); got != "gh" {
		t.Fatalf("toolBinary(github) = %q, want gh", got)
	}
	if got := toolBinary("bogus"); got != "" {
		t.Fatalf("toolBinary(bogus) = %q, want empty", got)
	}
}

// runBackend is a minimal Backend that only implements Run, delegating to a
// func so tests can control the probe result. Every other method panics; the
// installed() probe touches nothing else.
type runBackend struct {
	backend.Backend
	run func(argv []string) error
}

func (b runBackend) Run(_ context.Context, _ backend.ExecOpts, argv ...string) error {
	return b.run(argv)
}

func TestInstalled(t *testing.T) {
	// Probe succeeds -> installed.
	var got []string
	s := &session{ctx: context.Background(), b: runBackend{run: func(a []string) error {
		got = a
		return nil
	}}}
	ok, err := s.installed("github")
	if err != nil || !ok {
		t.Fatalf("installed(github) = %v, %v; want true, nil", ok, err)
	}
	if len(got) == 0 || !strings.Contains(got[len(got)-1], "command -v gh") {
		t.Fatalf("probe argv = %v; want it to run `command -v gh`", got)
	}

	// Non-zero exit (not found) -> skip gracefully, no error.
	s = &session{ctx: context.Background(), b: runBackend{run: func([]string) error {
		return &exec.ExitError{}
	}}}
	if ok, err := s.installed("codex"); ok || err != nil {
		t.Fatalf("installed(missing) = %v, %v; want false, nil", ok, err)
	}

	// Cancelled context -> abort (surface the error).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s = &session{ctx: ctx, b: runBackend{run: func([]string) error {
		return context.Canceled
	}}}
	if ok, err := s.installed("codex"); ok || err == nil {
		t.Fatalf("installed(cancelled) = %v, %v; want false, err", ok, err)
	}
}
