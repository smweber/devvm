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
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/spf13/cobra"
)

// copyOpts are the flags shared by cp-in and cp-out.
type copyOpts struct {
	recursive bool
	force     bool
	// asDir means DEST is a directory to copy into, created if missing. Set by
	// -t, a trailing slash, or the default destination.
	asDir bool
}

// copyArgs splits a cp-in/cp-out argument list into machine, sources and
// destination, following cp's rules plus two conveniences: DEST may be
// omitted (defaults to def), and -t DIR takes the destination so every
// positional after NAME is a source. Four or more positionals without -t are
// refused rather than treated cp-style (last is DEST): a variadic tail can't
// be shell-completed, and -t keeps every slot unambiguous.
func copyArgs(args []string, target, def string) (name string, srcs []string, dst string, asDir bool, err error) {
	name, rest := args[0], args[1:]
	switch {
	case target != "":
		return name, rest, target, true, nil
	case len(rest) == 1:
		return name, rest, def, true, nil
	case len(rest) == 2:
		if rest[1] == "" {
			return "", nil, "", false, fmt.Errorf("destination must not be empty")
		}
		return name, rest[:1], rest[1], strings.HasSuffix(rest[1], "/"), nil
	default:
		return "", nil, "", false, fmt.Errorf("copying several sources needs -t DIR to name the destination directory")
	}
}

// copyTargets renders the per-file success lines: "SRC -> personal:DEST/".
func copyTargets(w io.Writer, bases []string, dst string, asDir bool, format string) {
	if asDir && !strings.HasSuffix(dst, "/") {
		dst += "/"
	}
	for _, base := range bases {
		fmt.Fprintf(w, format+"\n", base, dst)
	}
}

func (a *App) cpInCmd() *cobra.Command {
	var o copyOpts
	var target string
	c := &cobra.Command{
		Use:   "cp-in NAME SOURCE [DEST]",
		Short: "Copy local files or directories into a machine",
		Long: "Copy local files into a machine; use -r for directories.\n" +
			"DEST is a guest path, relative to the login user's home unless absolute; it\n" +
			"defaults to the home directory. A DEST ending in / (or given with -t) is a\n" +
			"directory to copy into, created if missing. Several sources need -t DIR.\n" +
			"Existing files are never overwritten unless -f is given; a copy that would\n" +
			"overwrite anything is refused before a single file is written.\n" +
			"Files are copied as the normal guest user.",
		Example: "  devvm cp-in myvm ./notes.txt                  # -> ~/notes.txt\n" +
			"  devvm cp-in myvm ./notes.txt docs/            # into ~/docs, created if missing\n" +
			"  devvm cp-in myvm ./notes.txt docs/renamed.txt\n" +
			"  devvm cp-in -r myvm ./project /home/dev/project\n" +
			"  devvm cp-in myvm -t docs/ a.png b.png         # several sources\n" +
			"  devvm cp-in -f myvm ./notes.txt docs/         # overwrite ~/docs/notes.txt",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, srcs, dst, asDir, err := copyArgs(args, target, "~")
			if err != nil {
				return err
			}
			o.asDir = asDir
			return a.runCopyIn(cmd.Context(), name, srcs, dst, o)
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, incomplete string) ([]string, cobra.ShellCompDirective) {
			switch {
			case len(args) == 0:
				return a.completeMachines(cmd, args, incomplete)
			case cmd.Flags().Changed("target-directory"), len(args) == 1:
				return nil, cobra.ShellCompDirectiveDefault // local sources
			case len(args) == 2:
				return a.completeCopyGuestPath(cmd.Context(), args[0], incomplete)
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	c.Flags().BoolVarP(&o.recursive, "recursive", "r", false, "copy directories recursively")
	c.Flags().BoolVarP(&o.force, "force", "f", false, "overwrite existing guest files")
	c.Flags().StringVarP(&target, "target-directory", "t", "", "guest directory to copy every SOURCE into")
	c.RegisterFlagCompletionFunc("target-directory", func(cmd *cobra.Command, args []string, incomplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp // NAME not typed yet
		}
		return a.completeCopyGuestPath(cmd.Context(), args[0], incomplete)
	})
	return c
}

func (a *App) runCopyIn(ctx context.Context, name string, srcs []string, dst string, o copyOpts) error {
	if dst == "" {
		return fmt.Errorf("destination must not be empty")
	}
	if len(srcs) > 1 && !o.asDir {
		return fmt.Errorf("copying several sources needs a destination directory")
	}
	var abs, bases []string
	seen := map[string]string{}
	for _, src := range srcs {
		src, err := filepath.Abs(src)
		if err != nil {
			return err
		}
		info, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if info.IsDir() && !o.recursive {
			return fmt.Errorf("%s is a directory; use -r to copy it", src)
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported source file type: %s", src)
		}
		if filepath.Dir(src) == src {
			return fmt.Errorf("copying the filesystem root is not supported")
		}
		base := filepath.Base(src)
		if prev, dup := seen[base]; dup {
			return fmt.Errorf("%s and %s would both be copied as %s", prev, src, base)
		}
		seen[base] = src
		abs, bases = append(abs, src), append(bases, base)
	}
	_, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	// Stage one archive so both transports support directory trees without
	// relying on their different recursive-copy semantics or stdin handling.
	// It is written in Go rather than by the host tar: macOS bsdtar stores
	// quarantine and other xattrs as pax headers the guest's GNU tar warns
	// about, and several sources from different directories would need -C
	// juggling that bsdtar and GNU tar parse differently.
	tmp, err := os.MkdirTemp("", "devvm-cp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "payload.tar")
	if err := writeArchive(archive, abs, a.Stderr); err != nil {
		return fmt.Errorf("archive source: %w", err)
	}
	if err := copyArchive(ctx, b, name, archive, bases, dst, o, a.Stderr); err != nil {
		return err
	}
	copyTargets(a.Stderr, bases, dst, o.asDir || len(bases) > 1, "%s -> "+name+":%s")
	return nil
}

// copyStageDir is where the uploaded archive is staged in the guest. It must
// NOT be /tmp: `smolvm machine cp` writes into the machine's overlay upperdir,
// bypassing the running machine's mount namespace, and the machine mounts a
// tmpfs over /tmp (and /run, /dev). An archive uploaded there lands in the
// overlay's shadowed /tmp, invisible to every guest process, so the chmod and
// extract steps fail with "No such file" and the cleanup can't remove it
// either. /var/tmp is on the overlay, so uploads there are visible.
const copyStageDir = "/var/tmp"

// copyArchive uploads archive (holding one top-level entry per base) and runs
// the guest-side placement. The guest script's stderr is captured so a refusal
// (missing parent, would-overwrite list) comes back as the error text, prefixed
// with the machine name, rather than a bare "exit status 1".
func copyArchive(ctx context.Context, b backend.Backend, name, archive string, bases []string, dst string, o copyOpts, stderr io.Writer) error {
	var out bytes.Buffer
	if err := b.Run(ctx, backend.ExecOpts{Stdout: &out, Stderr: stderr},
		"mktemp", "-d", copyStageDir+"/devvm-cp-XXXXXXXXXX"); err != nil {
		return fmt.Errorf("create guest staging directory: %w", err)
	}
	stage := strings.TrimSpace(out.String())
	if !strings.HasPrefix(stage, copyStageDir+"/devvm-cp-") || strings.ContainsAny(strings.TrimPrefix(stage, copyStageDir+"/"), "/\n\r") {
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
	argv := append([]string{"sh", "-c", copyArchiveScript, "devvm-cp",
		stage, dst, strconv.FormatBool(o.asDir), strconv.FormatBool(o.force)}, bases...)
	var msg bytes.Buffer
	if err := b.Run(ctx, backend.ExecOpts{Stderr: &msg}, argv...); err != nil {
		if m := strings.TrimSpace(msg.String()); m != "" {
			return fmt.Errorf("%s: %s", name, m)
		}
		return fmt.Errorf("copy into destination: %w", err)
	}
	if msg.Len() > 0 {
		stderr.Write(msg.Bytes()) // tar or cp warnings on an otherwise successful copy
	}
	return nil
}

// Guest staging is owned by the login user. Even on smol (whose transport
// copies as root), extraction and the final copy run without root privileges.
//
// Placement is two-phase: extract into the stage, then check every staged
// path against its destination and refuse (listing the conflicts) before cp
// runs, so a refused copy has written nothing. Only -f skips the check.
// Everything user-supplied arrives as positional parameters, never
// interpolated into the script.
const copyArchiveScript = `set -eu
stage=$1; dest=$2; asdir=$3; force=$4; shift 4
mkdir "$stage/files"
tar -xf "$stage/payload.tar" -C "$stage/files" --no-same-owner
cd "$HOME"
case "$dest" in
  '~') dest=$HOME ;;
  '~/'*) dest="$HOME/${dest#\~/}" ;;
esac
if [ "$asdir" = true ]; then
  mkdir -p -- "$dest"
fi
into=false
if [ -d "$dest" ]; then
  into=true
elif [ ! -d "$(dirname -- "$dest")" ]; then
  echo "cannot copy to $dest: no such directory $(dirname -- "$dest") (relative paths resolve against the guest home; end DEST with / to create it)" >&2
  exit 1
fi
conflicts=
for base; do
  if [ "$into" = true ]; then target="$dest/$base"; else target=$dest; fi
  found=$(find "$stage/files/$base" -exec sh -c '
    root=$1; target=$2; shift 2
    for p; do
      t="$target${p#"$root"}"
      if [ -d "$p" ] && [ ! -L "$p" ]; then
        [ ! -e "$t" ] || [ -d "$t" ] || printf "  %s\n" "$t"
      elif [ -e "$t" ] || [ -L "$t" ]; then
        printf "  %s\n" "$t"
      fi
    done' sh "$stage/files/$base" "$target" {} +)
  [ -z "$found" ] || conflicts="$conflicts$found
"
done
if [ -n "$conflicts" ] && [ "$force" != true ]; then
  printf 'refusing to overwrite (use -f):\n%s' "$conflicts" >&2
  exit 1
fi
for base; do
  cp -R -- "$stage/files/$base" "$dest"
done
`

// copyOutScript is the guest half of cp-out: validate one source and stream it
// as a tar on stdout. Kept beside copyArchiveScript so the ~ handling stays in
// one place.
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
  echo "$src is a directory; use -r to copy it" >&2; exit 1
fi
if [ ! -f "$src" ] && [ ! -d "$src" ] && [ ! -L "$src" ]; then
  echo "$src is missing or has an unsupported file type" >&2; exit 1
fi
exec tar -cf - -C "$(dirname -- "$src")" -- "$(basename -- "$src")"
`
