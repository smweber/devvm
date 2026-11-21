package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
)

// smol defaults, matching the globals at the top of bin/devvm.
const (
	smolImage = "ubuntu:24.04"
	smolCPUs  = 4
	// SmolDefaultDiskGiB is the fallback disk size when neither a flag nor
	// the template sets one.
	SmolDefaultDiskGiB = 50
)

// smolBackend drives one local smol VM. row is the `smolvm machine ls`
// row the record was resolved from (nil when built from a bare record):
// what `status NAME` shows sizes from without a second listing.
type smolBackend struct {
	m     *config.Machine
	paths config.Paths
	row   *SmolMachine
}

func (b *smolBackend) Kind() string { return config.BackendSmol }

// ErrNoSmolvm is every refusal for a host without smolvm on PATH, naming
// smolvm's own installer.
var ErrNoSmolvm = errors.New("smolvm is not installed; install it with: curl -sSL https://smolmachines.com/install.sh | bash")

func needSmolvm() error {
	if _, err := exec.LookPath("smolvm"); err != nil {
		return ErrNoSmolvm
	}
	return nil
}

// SmolAvailable reports whether the smolvm CLI is present at all.
func SmolAvailable() bool { return needSmolvm() == nil }

// smolCmd runs a state-changing smolvm command with stderr passed through, so
// a failure surfaces smolvm's message instead of a bare "exit status 1".
// (The `machine ls` probe stays quiet on purpose.)
func smolCmd(args ...string) error {
	cmd := exec.Command("smolvm", args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SmolMachine is one entry from `smolvm machine ls --json`: everything devvm
// knows about a local smol VM, from smolvm's own record (config.md §2, §3).
// Sizes are smolvm's, read for running and stopped VMs alike, so devvm
// stores none. Pinned against a real smolvm 1.16.1 (running and stopped
// rows, testdata/smolvm-machine-ls*.json); labels as 1.35.5 prints them
// (an object of strings, `{}` when none). A smolvm without labels in its
// listing reads as every VM unlabelled.
type SmolMachine struct {
	Name      string            `json:"name"`
	State     string            `json:"state"`
	MemoryMiB int               `json:"memory_mib"`
	StorageGB int               `json:"storage_gb"`
	CPUs      int               `json:"cpus"`
	Labels    map[string]string `json:"labels"`
}

// Running reports whether smolvm says the VM is up.
func (s SmolMachine) Running() bool { return s.State == "running" }

// smolLabel is devvm's ownership label on a smol VM, the same one a
// provider server carries: `create` sets it, and a VM without it is one
// devvm did not create.
var smolLabel = provider.LabelManaged + "=" + provider.LabelManagedValue

// Labelled reports whether the VM carries devvm's label (devvm=true): the
// smol source's whole ownership rule (config.md §2).
func (s SmolMachine) Labelled() bool {
	return s.Labels[provider.LabelManaged] == provider.LabelManagedValue
}

// parseSmolList decodes `smolvm machine ls --json`.
func parseSmolList(out []byte) ([]SmolMachine, error) {
	var ms []SmolMachine
	if err := json.Unmarshal(out, &ms); err != nil {
		return nil, fmt.Errorf("smolvm machine ls: unexpected output: %w", err)
	}
	return ms, nil
}

// smolLs runs the listing; a var so tests can stand in for smolvm.
var smolLs = func() ([]byte, error) {
	out, err := exec.Command("smolvm", "machine", "ls", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("smolvm machine ls: %w", err)
	}
	return out, nil
}

// smolList lists every VM smolvm knows, reporting failures (resolve wants
// the real cause, not "no such machine").
func smolList() ([]SmolMachine, error) {
	if err := needSmolvm(); err != nil {
		return nil, err
	}
	out, err := smolLs()
	if err != nil {
		return nil, err
	}
	return parseSmolList(out)
}

// SmolList returns all machines smolvm knows about (empty if smolvm is absent
// or the listing fails, like the old jq path): the listing `status` takes
// once per snapshot.
func SmolList() ([]SmolMachine, error) {
	ms, err := smolList()
	if err != nil {
		return nil, nil
	}
	return ms, nil
}

// smolLookup is one `smolvm machine ls --json`, picked down to one name:
// existence, state, sizes and labels in one subprocess (it replaced a
// `machine status --name` probe, so resolution costs no more than before).
func smolLookup(name string) (SmolMachine, bool, error) {
	ms, err := smolList()
	if err != nil {
		return SmolMachine{}, false, err
	}
	for _, sm := range ms {
		if sm.Name == name {
			return sm, true, nil
		}
	}
	return SmolMachine{}, false, nil
}

// SmolExists reports whether smolvm knows a machine by this name, labelled
// or not (create's refusal). Quiet: any failure reads as "no".
func SmolExists(name string) bool {
	_, ok, err := smolLookup(name)
	return err == nil && ok
}

// SmolLookup is smolLookup for callers outside the package that must tell
// "not listed" from "smolvm did not answer" (ports' deleted-meanwhile
// check).
func SmolLookup(name string) (SmolMachine, bool, error) { return smolLookup(name) }

// SmolRow is the `smolvm machine ls` row b was resolved from, if b is a
// smol backend built by discovery (status NAME's sizes, with no second
// listing).
func SmolRow(b Backend) (SmolMachine, bool) {
	if sb, ok := b.(*smolBackend); ok && sb.row != nil {
		return *sb.row, true
	}
	return SmolMachine{}, false
}

func (b *smolBackend) Exists() (bool, error) {
	_, ok, err := smolLookup(b.m.Name)
	return ok, err
}

func (b *smolBackend) PowerStart() error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return smolCmd("machine", "start", "--name", b.m.Name)
}

func (b *smolBackend) PowerStop() error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return smolCmd("machine", "stop", "--name", b.m.Name)
}

func (b *smolBackend) PowerDelete() error {
	if err := needSmolvm(); err != nil {
		return err
	}
	// --force skips smolvm's own confirmation; the host CLI already confirmed
	// (and its second prompt would otherwise eat our stdin and leave the disk).
	return smolCmd("machine", "delete", "--force", "--name", b.m.Name)
}

func (b *smolBackend) Status() (State, error) {
	sm, ok, err := smolLookup(b.m.Name)
	if err != nil {
		return State{}, err
	}
	st := State{Name: b.m.Name, Backend: config.BackendSmol, Raw: "not created"}
	if ok {
		st.Exists, st.Running, st.Raw = true, sm.Running(), sm.State
	}
	return st, nil
}

// Refresh re-asks the smol discovery source about this VM: a fresh copy of
// the record, and whether it runs. The session daemon calls it at start and
// before every reconnect dial (config.md §2): a VM still listed with
// devvm's label is the one it serves. One gone, or listed without the
// label (deleted and recreated under the same name outside devvm while the
// daemon slept), reads as not running with a nil record, so it is never
// dialled and nothing is installed into it. The record itself is never
// mutated (other goroutines may hold it).
func (b *smolBackend) Refresh() (*config.Machine, bool, error) {
	sm, ok, err := smolLookup(b.m.Name)
	if err != nil {
		return nil, false, err
	}
	if !ok || !sm.Labelled() {
		return nil, false, nil // gone, or not devvm's: as un-dialable as a stopped VM
	}
	fresh := *b.m
	fresh.SetManaged(true)
	return &fresh, sm.Running(), nil
}

// CopiesAsRoot: see Copy.
func (b *smolBackend) CopiesAsRoot() bool { return true }

// Copy uploads via `smolvm machine cp`, which writes into the machine's
// overlay upperdir rather than through the running machine's mounts. Two
// consequences for callers: a relative guestDst resolves against / (not the
// login user's home), and anything under a guest tmpfs (/tmp, /run, /dev) is
// shadowed and never visible inside the machine. Pass absolute, overlay-backed
// paths only. Files are created as root. Output is shown only on failure.
func (b *smolBackend) Copy(hostSrc, guestDst string) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return quietHost([]string{"smolvm", "machine", "cp", hostSrc, b.m.Name + ":" + guestDst})
}

// CopyOut downloads via `smolvm machine cp NAME:SRC DST`, the mirror of
// Copy, read as root. Unlike an upload it reads the guest's live filesystem
// from inside the guest (shown on real smolvm: a file just written needs no
// sync, and even one on the guest's /tmp tmpfs is read). Output is shown
// only on failure.
func (b *smolBackend) CopyOut(ctx context.Context, guestSrc, hostDst string) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return quietHostContext(ctx, []string{"smolvm", "machine", "cp", b.m.Name + ":" + guestSrc, hostDst})
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
//
// An unmanaged VM (one devvm did not create, config.md §2) gets a plain
// `machine exec` of argv as smolvm's own default user: no hostname
// re-assert, no sudo to a `dev` user it has no reason to have, no login
// wrap (its shell may not be bash). ExecOpts.User and Login are ignored
// there, and Env rides smolvm's own -e flags.
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
	if !b.m.Managed() {
		for _, kv := range envAssignments(o.Env) {
			h = append(h, "-e", kv)
		}
		h = append(h, "--name", b.m.Name, "--")
		return append(h, argv...)
	}
	h = append(h, "--name", b.m.Name, "--")
	return append(h, b.guestArgv(o, argv)...)
}

// guestArgv wraps argv to run as the requested user with the guest env.
// SMOLVM_GUEST is set machine-wide at create; re-asserting it here is harmless
// and keeps parity with the old dev-user wrapper.
//
// The leading `hostname` re-assert is load-bearing (the old smol_run had it
// too): smolvm's init resets the runtime hostname to "container" on every boot
// and ignores /etc/hostname, so without it every sudo below — i.e. every
// non-root exec — spews "unable to resolve host container". The exec enters the
// guest as root, so this is the one spot it can run before the user drop.
func (b *smolBackend) guestArgv(o ExecOpts, argv []string) []string {
	out := []string{"sh", "-c", `hostname "$1" 2>/dev/null; shift; exec "$@"`, "_", b.m.Name}
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

// ErrNoSmolLabel is create's refusal on a smolvm whose `machine create`
// has no --label (1.16.1 has it): devvm owns only the VMs it labels.
var ErrNoSmolLabel = errors.New("this smolvm's 'machine create' has no --label, which devvm needs to mark the VMs it makes; upgrade smolvm: curl -sSL https://smolmachines.com/install.sh | bash")

// SmolCreate provisions a new microVM (net-enabled, SMOLVM_GUEST=1, devvm's
// label) and starts it. Mirrors the `smolvm machine create` invocation in
// create_machine. A diskGiB <= 0 falls back to SmolDefaultDiskGiB, so a
// pre-disk conf still boots. A smolvm that does not know --label is told
// from its own stderr, clap's unknown-argument line for that flag
// (ErrNoSmolLabel), so no version is probed up front; any other failure,
// a refused label value included, is smolvm's own.
func SmolCreate(name string, memoryMiB, diskGiB int) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	if diskGiB <= 0 {
		diskGiB = SmolDefaultDiskGiB
	}
	args := []string{
		"machine", "create",
		"--name", name,
		"--image", smolImage,
		"--net",
		"--cpus", fmt.Sprint(smolCPUs),
		"--mem", fmt.Sprint(memoryMiB),
		"--storage", fmt.Sprint(diskGiB),
		"--env", "SMOLVM_GUEST=1",
		"--label", smolLabel,
	}
	fmt.Printf("+ smolvm %v\n", args)
	var stderr bytes.Buffer
	cmd := exec.Command("smolvm", args...)
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "unexpected argument '--label'") {
			return ErrNoSmolLabel
		}
		return err
	}
	return smolCmd("machine", "start", "--name", name)
}

// SmolLabel puts devvm's label on a VM devvm did not create (`setup
// --adopt`): `machine update --label`, which smolvm 1.35.5 has, on a
// running VM too (1.16.1 has no update --label: it fails with smolvm's own
// message).
func SmolLabel(name string) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return smolCmd("machine", "update", "--name", name, "--label", smolLabel)
}
