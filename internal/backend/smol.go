package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"

	"github.com/smweber/devvm/internal/config"
)

// smol defaults, matching the globals at the top of bin/devvm.
const (
	smolImage      = "ubuntu:24.04"
	smolCPUs       = 4
	smolStorageGiB = 50
)

type smolBackend struct{ m *config.Machine }

func (b *smolBackend) Kind() string { return config.BackendSmol }

func needSmolvm() error {
	if _, err := exec.LookPath("smolvm"); err != nil {
		return errors.New("smolvm is not installed; run bootstrap.sh on the host")
	}
	return nil
}

// SmolAvailable reports whether the smolvm CLI is present at all.
func SmolAvailable() bool { return needSmolvm() == nil }

// SmolExists reports whether smolvm knows a machine by this name (used before a
// conf exists, e.g. create and the unregistered-VM fallback).
func SmolExists(name string) bool {
	if !SmolAvailable() {
		return false
	}
	return exec.Command("smolvm", "machine", "status", "--name", name).Run() == nil
}

// SmolMachine is one entry from `smolvm machine ls --json`.
type SmolMachine struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// SmolList returns all machines smolvm knows about (empty if smolvm is absent).
func SmolList() ([]SmolMachine, error) {
	if !SmolAvailable() {
		return nil, nil
	}
	out, err := exec.Command("smolvm", "machine", "ls", "--json").Output()
	if err != nil {
		return nil, nil // treat a failing ls as "no machines", like the old jq path
	}
	var ms []SmolMachine
	if err := json.Unmarshal(out, &ms); err != nil {
		return nil, nil
	}
	return ms, nil
}

func smolState(name string) string {
	ms, _ := SmolList()
	for _, m := range ms {
		if m.Name == name {
			return m.State
		}
	}
	return "not created"
}

func (b *smolBackend) Exists() (bool, error) {
	if err := needSmolvm(); err != nil {
		return false, err
	}
	return SmolExists(b.m.Name), nil
}

func (b *smolBackend) PowerStart() error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return exec.Command("smolvm", "machine", "start", "--name", b.m.Name).Run()
}

func (b *smolBackend) PowerStop() error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return exec.Command("smolvm", "machine", "stop", "--name", b.m.Name).Run()
}

func (b *smolBackend) PowerDelete() error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return exec.Command("smolvm", "machine", "delete", "--name", b.m.Name).Run()
}

func (b *smolBackend) Status() (State, error) {
	if err := needSmolvm(); err != nil {
		return State{}, err
	}
	st := smolState(b.m.Name)
	return State{
		Name:    b.m.Name,
		Backend: config.BackendSmol,
		Exists:  st != "not created",
		Running: st == "running",
		Raw:     st,
	}, nil
}

func (b *smolBackend) Copy(hostSrc, guestDst string) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return exec.Command("smolvm", "machine", "cp", hostSrc, b.m.Name+":"+guestDst).Run()
}

func (b *smolBackend) Run(ctx context.Context, o ExecOpts, argv ...string) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return runHost(ctx, o, b.execArgv(o, false, argv))
}

func (b *smolBackend) Spawn(ctx context.Context, o ExecOpts, argv ...string) (*Session, error) {
	if err := needSmolvm(); err != nil {
		return nil, err
	}
	return spawnHost(ctx, b.execArgv(o, true, argv))
}

// execArgv builds the full `smolvm machine exec` host command. spawn forces the
// `-i --stream` bytes-pipe mode used for the persistent agent (mux-style).
func (b *smolBackend) execArgv(o ExecOpts, spawn bool, argv []string) []string {
	h := []string{"smolvm", "machine", "exec"}
	switch {
	case spawn:
		h = append(h, "-i", "--stream")
	case o.TTY:
		h = append(h, "-it", "-e", "TERM="+term())
	case o.Stream:
		h = append(h, "--stream")
	}
	h = append(h, "--name", b.m.Name, "--")
	return append(h, b.guestArgv(o, argv)...)
}

// guestArgv wraps argv to run as the requested user with the guest env. Unlike
// the old smol_run it drops the per-exec `hostname` call (prereqs set
// /etc/hostname once). SMOLVM_GUEST is set machine-wide at create; re-asserting
// it here is harmless and keeps parity with the old dev-user wrapper.
func (b *smolBackend) guestArgv(o ExecOpts, argv []string) []string {
	var out []string
	if o.user() != "root" {
		out = append(out, "sudo", "-u", o.user(), "-H")
	}
	out = append(out, "env", "SMOLVM_GUEST=1")
	out = append(out, envAssignments(o.Env)...)
	if o.Login {
		out = append(out, "bash", "-lc", `exec "$@"`, "_")
	}
	return append(out, argv...)
}

// SmolCreate provisions a new microVM (net-enabled, SMOLVM_GUEST=1) and starts
// it. Mirrors the `smolvm machine create` invocation in create_machine.
func SmolCreate(name string, memoryMiB int) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	args := []string{
		"machine", "create",
		"--name", name,
		"--image", smolImage,
		"--net",
		"--cpus", fmt.Sprint(smolCPUs),
		"--mem", fmt.Sprint(memoryMiB),
		"--storage", fmt.Sprint(smolStorageGiB),
		"--env", "SMOLVM_GUEST=1",
	}
	fmt.Printf("+ smolvm %v\n", args)
	if err := exec.Command("smolvm", args...).Run(); err != nil {
		return err
	}
	return exec.Command("smolvm", "machine", "start", "--name", name).Run()
}
