package cli

import (
	"bytes"
	"sync"

	"github.com/smweber/devvm/internal/config"
)

// writeEntry appends m as a [machines.*] table to the test's devvm.toml,
// through the same structural write `config add` uses (the tests' stand-in
// for the old Machine.Save).
func writeEntry(p config.Paths, m *config.Machine) error {
	return config.AppendEntry(p, m.Name, config.EntryOf(m))
}

// okSnap adapts a snapshot that cannot fail to watchStatus's.
func okSnap(f func() string) func() (string, error) {
	return func() (string, error) { return f(), nil }
}

// syncBuffer is a bytes.Buffer safe to write from a watch goroutine while
// the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
