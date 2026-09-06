package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/spf13/cobra"
)

func (a *App) cpOutCmd() *cobra.Command {
	var recursive bool
	c := &cobra.Command{
		Use:   "cp-out NAME SOURCE DEST",
		Short: "Copy a guest file or directory onto the host",
		Long: "Copy a guest file onto the host; use -r for directories.\n" +
			"SOURCE is relative to the guest user's home unless absolute.\n" +
			"Existing destination directories receive SOURCE's basename; files are overwritten.",
		Example: "  devvm cp-out myvm notes.txt ./notes.txt\n  devvm cp-out -r myvm project ./backup",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[1] == "" || args[2] == "" {
				return fmt.Errorf("source and destination must not be empty")
			}
			_, b, err := a.resolveLive(args[0])
			if err != nil {
				return err
			}
			return copyOut(cmd.Context(), b, args[1], args[2], recursive, a.Stderr)
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
			switch len(args) {
			case 0:
				return a.completeMachines(cmd, args, prefix)
			case 1:
				return a.completeCopyGuestPath(cmd.Context(), args[0], prefix)
			case 2:
				return nil, cobra.ShellCompDirectiveDefault
			default:
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
		},
	}
	c.Flags().BoolVarP(&recursive, "recursive", "r", false, "copy a directory recursively")
	return c
}

func copyOut(ctx context.Context, b backend.Backend, src, dst string, recursive bool, stderr io.Writer) error {
	if src == "" || dst == "" {
		return fmt.Errorf("source and destination must not be empty")
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
	archive := filepath.Join(tmp, "payload.tar")
	f, err := os.Create(archive)
	if err != nil {
		return err
	}
	// Streaming stdout is binary-safe on both transports. No guest staging
	// files or root access are needed, and failed downloads never reach DEST.
	err = b.Run(ctx, backend.ExecOpts{Stream: true, Stdin: strings.NewReader(""), Stdout: f, Stderr: stderr},
		"sh", "-c", copyOutScript, "devvm-cp-out", src, strconv.FormatBool(recursive))
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("download source: %w", err)
	}
	if closeErr != nil {
		return closeErr
	}
	extracted := filepath.Join(tmp, "files")
	if err := os.Mkdir(extracted, 0700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "tar", "-xf", archive, "-C", extracted, "--no-same-owner")
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("extract download: %w", err)
	}
	entries, err := os.ReadDir(extracted)
	if err != nil {
		return err
	}
	if len(entries) != 1 {
		return fmt.Errorf("expected one source in downloaded archive, got %d", len(entries))
	}
	cmd = exec.CommandContext(ctx, "cp", "-R", "--", filepath.Join(extracted, entries[0].Name()), dst)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("copy into destination: %w", err)
	}
	return nil
}

const copyOutScript = `set -eu
cd "$HOME"
src=$1
case "$src" in
  '~') src=$HOME ;;
  '~/'*) src="$HOME/${src#\~/}" ;;
esac
src=$(realpath -ms -- "$src")
[ "$src" != / ] || { echo 'copying the filesystem root is not supported' >&2; exit 1; }
if [ -d "$src" ] && [ ! -L "$src" ] && [ "$2" != true ]; then
  echo 'source is a directory; use -r to copy it' >&2; exit 1
fi
if [ ! -f "$src" ] && [ ! -d "$src" ] && [ ! -L "$src" ]; then
  echo 'source is missing or has an unsupported file type' >&2; exit 1
fi
exec tar -cf - -C "$(dirname -- "$src")" -- "$(basename -- "$src")"
`
