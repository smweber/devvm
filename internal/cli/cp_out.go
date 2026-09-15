package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/spf13/cobra"
)

func (a *App) cpOutCmd() *cobra.Command {
	var o copyOpts
	var target string
	c := &cobra.Command{
		Use:   "cp-out NAME SOURCE [DEST]",
		Short: "Copy guest files or directories onto the host",
		Long: "Copy guest files onto the host; use -r for directories.\n" +
			"SOURCE is relative to the guest user's home unless absolute. DEST is a host\n" +
			"path defaulting to the current directory. A DEST ending in / (or given with\n" +
			"-t) is a directory to copy into, created if missing. Several sources need\n" +
			"-t DIR. Existing files are never overwritten unless -f is given; a copy that\n" +
			"would overwrite anything is refused before a single file is written.",
		Example: "  devvm cp-out myvm notes.txt                   # -> ./notes.txt\n" +
			"  devvm cp-out myvm notes.txt ~/Desktop/\n" +
			"  devvm cp-out -r myvm project ./backup\n" +
			"  devvm cp-out myvm -t ./downloads a.log b.log  # several sources\n" +
			"  devvm cp-out -f myvm notes.txt                # overwrite ./notes.txt",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, srcs, dst, asDir, err := copyArgs(args, target, ".")
			if err != nil {
				return err
			}
			o.asDir = asDir
			for _, src := range srcs {
				if src == "" {
					return fmt.Errorf("source must not be empty")
				}
			}
			_, b, err := a.resolveLive(name)
			if err != nil {
				return err
			}
			return copyOut(cmd.Context(), b, name, srcs, dst, o, a.Stderr)
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
			switch {
			case len(args) == 0:
				return a.completeMachines(cmd, args, prefix)
			case cmd.Flags().Changed("target-directory"), len(args) == 1:
				return a.completeCopyGuestPath(cmd.Context(), args[0], prefix)
			case len(args) == 2:
				return nil, cobra.ShellCompDirectiveDefault // local destination
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	c.Flags().BoolVarP(&o.recursive, "recursive", "r", false, "copy directories recursively")
	c.Flags().BoolVarP(&o.force, "force", "f", false, "overwrite existing host files")
	c.Flags().StringVarP(&target, "target-directory", "t", "", "host directory to copy every SOURCE into")
	c.RegisterFlagCompletionFunc("target-directory", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveFilterDirs
	})
	return c
}

// copyOut downloads each guest source as a tar stream, then checks every
// path against the destination before extracting anything, so a refused copy
// has written nothing. One exec per source: sequential one-shot execs are fine
// on smol (the concurrency limit is on parallel ones), and it keeps the guest
// script free of tar -C juggling that differs between implementations.
func copyOut(ctx context.Context, b backend.Backend, name string, srcs []string, dst string, o copyOpts, stderr io.Writer) error {
	if len(srcs) == 0 || dst == "" {
		return fmt.Errorf("source and destination must not be empty")
	}
	if len(srcs) > 1 && !o.asDir {
		return fmt.Errorf("copying several sources needs a destination directory")
	}
	dst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "devvm-cp-out-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var archives []string
	var entries []archiveEntry
	seen := map[string]string{}
	for i, src := range srcs {
		archive := filepath.Join(tmp, strconv.Itoa(i)+".tar")
		if err := downloadArchive(ctx, b, name, src, archive, o.recursive, stderr); err != nil {
			return err
		}
		got, err := readArchive(archive)
		if err != nil {
			return fmt.Errorf("read download of %s: %w", src, err)
		}
		if len(got) != 1 {
			return fmt.Errorf("expected one source in the download of %s, got %d", src, len(got))
		}
		if prev, dup := seen[got[0].name]; dup {
			return fmt.Errorf("%s and %s would both be copied as %s", prev, src, got[0].name)
		}
		seen[got[0].name] = src
		archives, entries = append(archives, archive), append(entries, got[0])
	}
	if o.asDir {
		if err := os.MkdirAll(dst, 0755); err != nil {
			return err
		}
	}
	into := false
	if info, err := os.Stat(dst); err == nil && info.IsDir() {
		into = true
	} else if _, err := os.Stat(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("cannot copy to %s: no such directory %s (end DEST with / to create it)", dst, filepath.Dir(dst))
	}
	roots := make([]string, len(entries))
	var conflicts []string
	for i, e := range entries {
		roots[i] = dst
		if into {
			roots[i] = filepath.Join(dst, e.name)
		}
		for _, p := range e.paths {
			t := filepath.Join(roots[i], filepath.FromSlash(p.rel))
			info, err := os.Lstat(t)
			if err != nil {
				continue
			}
			if !p.isDir || !info.IsDir() { // merging into an existing directory is fine
				conflicts = append(conflicts, t)
			}
		}
	}
	if len(conflicts) > 0 && !o.force {
		return fmt.Errorf("refusing to overwrite (use -f):\n  %s", strings.Join(conflicts, "\n  "))
	}
	var bases []string
	for i, e := range entries {
		if err := extractArchive(archives[i], map[string]string{e.name: roots[i]}, o.force); err != nil {
			return fmt.Errorf("copy %s into destination: %w", srcs[i], err)
		}
		bases = append(bases, name+":"+srcs[i])
	}
	copyTargets(stderr, bases, dst, o.asDir || len(bases) > 1, "%s -> %s")
	return nil
}

// downloadArchive streams one guest source to a host tar file. Streaming
// stdout is binary-safe on both transports, needs no guest staging or root,
// and a failed download never touches DEST. The guest script's stderr is
// captured so its refusal reads as the error, prefixed with the machine name.
func downloadArchive(ctx context.Context, b backend.Backend, name, src, archive string, recursive bool, stderr io.Writer) error {
	f, err := os.Create(archive)
	if err != nil {
		return err
	}
	var msg bytes.Buffer
	err = b.Run(ctx, backend.ExecOpts{Stream: true, Stdin: strings.NewReader(""), Stdout: f, Stderr: &msg},
		"sh", "-c", copyOutScript, "devvm-cp-out", src, strconv.FormatBool(recursive))
	closeErr := f.Close()
	if err != nil {
		if m := strings.TrimSpace(msg.String()); m != "" {
			return fmt.Errorf("%s: %s", name, m)
		}
		return fmt.Errorf("download %s: %w", src, err)
	}
	if msg.Len() > 0 {
		stderr.Write(msg.Bytes())
	}
	return closeErr
}
