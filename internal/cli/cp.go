package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/spf13/cobra"
)

func (a *App) cpCmd() *cobra.Command {
	var recursive bool
	c := &cobra.Command{
		Use:   "cp NAME SOURCE DEST",
		Short: "Copy a local file or directory into a machine",
		Long: "Copy a local file into a machine; use -r for directories.\n" +
			"DEST is a guest path, relative to the login user's home unless absolute.\n" +
			"Existing directories receive SOURCE's basename; existing files are overwritten.\n" +
			"Files are copied as the normal guest user. Downloads are not supported.",
		Example: "  devvm cp myvm ./notes.txt notes.txt\n  devvm cp -r myvm ./project /home/dev/project",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runCopy(cmd.Context(), args[0], args[1], args[2], recursive)
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, incomplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return a.completeMachines(cmd, args, incomplete)
			}
			if len(args) == 1 {
				return nil, cobra.ShellCompDirectiveDefault
			}
			if len(args) == 2 {
				return a.completeCopyDestination(cmd.Context(), args[0], incomplete)
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	c.Flags().BoolVarP(&recursive, "recursive", "r", false, "copy a directory recursively")
	return c
}

func (a *App) runCopy(ctx context.Context, name, src, dst string, recursive bool) error {
	if dst == "" {
		return fmt.Errorf("destination must not be empty")
	}
	src, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() && !recursive {
		return fmt.Errorf("%s is a directory; use -r to copy it", src)
	}
	if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("unsupported source file type: %s", src)
	}
	if filepath.Dir(src) == src {
		return fmt.Errorf("copying the filesystem root is not supported")
	}
	_, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	// Stage one archive so both transports support directory trees without
	// relying on their different recursive-copy semantics or stdin handling.
	tmp, err := os.MkdirTemp("", "devvm-cp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "payload.tar")
	cmd := exec.CommandContext(ctx, "tar", "-cf", archive, "-C", filepath.Dir(src), "--", filepath.Base(src))
	cmd.Stderr = a.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("archive source: %w", err)
	}
	return copyArchive(ctx, b, archive, filepath.Base(src), dst, a.Stderr)
}

func copyArchive(ctx context.Context, b backend.Backend, archive, base, dst string, stderr io.Writer) error {
	var out bytes.Buffer
	if err := b.Run(ctx, backend.ExecOpts{Stdout: &out, Stderr: stderr},
		"mktemp", "-d", "/tmp/devvm-cp-XXXXXXXXXX"); err != nil {
		return fmt.Errorf("create guest staging directory: %w", err)
	}
	stage := strings.TrimSpace(out.String())
	if !strings.HasPrefix(stage, "/tmp/devvm-cp-") || strings.ContainsAny(strings.TrimPrefix(stage, "/tmp/"), "/\n\r") {
		return fmt.Errorf("unexpected guest staging path %q", stage)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := b.Run(cleanupCtx, backend.ExecOpts{Stderr: stderr}, "rm", "-rf", "--", stage); err != nil {
			fmt.Fprintf(stderr, "devvm: could not clean up %s: %v\n", stage, err)
		}
	}()
	if err := b.Copy(archive, stage+"/payload.tar"); err != nil {
		return fmt.Errorf("upload archive: %w", err)
	}
	if b.Kind() == config.BackendSmol {
		// smolvm cp creates files as root. The private parent directory still
		// restricts access to the guest user while they read the archive.
		if err := b.Run(ctx, backend.ExecOpts{User: "root", Stderr: stderr},
			"chmod", "0644", stage+"/payload.tar"); err != nil {
			return fmt.Errorf("make archive readable: %w", err)
		}
	}
	if err := b.Run(ctx, backend.ExecOpts{Stderr: stderr},
		"sh", "-c", copyArchiveScript, "devvm-cp", stage, base, dst); err != nil {
		return fmt.Errorf("copy into destination: %w", err)
	}
	return nil
}

// Guest staging is owned by the login user. Even on smol (whose transport
// copies as root), extraction and the final copy run without root privileges.
const copyArchiveScript = `set -eu
stage=$1
base=$2
dest=$3
mkdir "$stage/files"
tar -xf "$stage/payload.tar" -C "$stage/files" --no-same-owner
cd "$HOME"
case "$dest" in
  '~') dest=$HOME ;;
  '~/'*) dest="$HOME/${dest#\~/}" ;;
esac
cp -R -- "$stage/files/$base" "$dest"
`
