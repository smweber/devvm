package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/smweber/devvm/internal/keys"
)

// runKeys handles the one-shot guest-side authorized_keys operations the host
// invokes over a single exec: list | add | cleanup | revoke. All key logic is
// in internal/keys; this is just the filesystem glue that runs as the dev user.
func runKeys(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: devvm-agent keys {list|add|cleanup|revoke PATTERN}")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		keysList()
	case "add":
		keysAdd()
	case "cleanup":
		keysCleanup()
	case "revoke":
		if len(args) < 2 {
			fatal("keys revoke needs a PATTERN")
		}
		keysRevoke(args[1])
	default:
		fatal("unknown keys op: " + args[0])
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "devvm-agent: "+msg)
	os.Exit(1)
}

func sshDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal("no home directory: " + err.Error())
	}
	return filepath.Join(home, ".ssh")
}

func authKeysPath() string { return filepath.Join(sshDir(), "authorized_keys") }

func readAuthKeys() []string {
	data, err := os.ReadFile(authKeysPath())
	if err != nil {
		return nil // missing == empty
	}
	return keys.Split(string(data))
}

func writeAuthKeys(lines []string) {
	dir := sshDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fatal(err.Error())
	}
	if err := os.WriteFile(authKeysPath(), []byte(keys.Join(lines)), 0o600); err != nil {
		fatal(err.Error())
	}
}

// ownIDs returns the identities of the machine's own keys (~/.ssh/id_*.pub),
// which cleanup/add strip from authorized_keys.
func ownIDs() map[string]bool {
	var pubs []string
	matches, _ := filepath.Glob(filepath.Join(sshDir(), "id_*.pub"))
	for _, m := range matches {
		if data, err := os.ReadFile(m); err == nil {
			pubs = append(pubs, strings.TrimSpace(string(data)))
		}
	}
	return keys.IDs(pubs)
}

func keysList() {
	lines := readAuthKeys()
	if len(lines) == 0 {
		fmt.Println("(no authorized_keys)")
		return
	}
	for _, s := range keys.List(lines) {
		fmt.Println(s)
	}
}

// keysAdd reads new key lines from stdin, appends them, and dedups (by material,
// dropping the machine's own keys). Matches push_authorized_keys + cleanup.
func keysAdd() {
	var incoming []string
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			incoming = append(incoming, line)
		}
	}
	existing := readAuthKeys()
	kept, _, _ := keys.Dedup(append(existing, incoming...), ownIDs())
	writeAuthKeys(kept)
	fmt.Printf("devvm: processed %d key(s)\n", len(incoming))
}

func keysCleanup() {
	lines := readAuthKeys()
	if len(lines) == 0 {
		fmt.Println("devvm: no authorized_keys")
		return
	}
	kept, dup, own := keys.Dedup(lines, ownIDs())
	writeAuthKeys(kept)
	fmt.Printf("devvm: removed %d duplicate and %d machine-owned key line(s)\n", dup, own)
}

// keysRevoke removes the single key matching PATTERN. Protected (host-owned) key
// identities arrive on stdin, one public-key line each, so the caller's own key
// can never be revoked.
func keysRevoke(pattern string) {
	var protectedPubs []string
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			protectedPubs = append(protectedPubs, line)
		}
	}
	lines := readAuthKeys()
	if len(lines) == 0 {
		fmt.Println("devvm: no authorized_keys")
		return
	}
	idx, summary, err := keys.Revoke(lines, pattern, keys.IDs(protectedPubs))
	if err != nil {
		fatal(err.Error())
	}
	kept := append(lines[:idx], lines[idx+1:]...)
	writeAuthKeys(kept)
	fmt.Printf("devvm: removed %s\n", summary)
}
