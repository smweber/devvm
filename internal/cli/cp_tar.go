package cli

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// writeArchive tars each source (a file, symlink or directory tree) under its
// basename. Symlinks are stored as links, never followed; modes and mtimes are
// kept; ownership and xattrs are deliberately not (the guest extracts with
// --no-same-owner, and macOS quarantine metadata must not travel). Entries of
// other types (sockets, devices) are skipped with a note, as tar would.
func writeArchive(archive string, srcs []string, stderr io.Writer) error {
	f, err := os.Create(archive)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	for _, src := range srcs {
		if err := archiveTree(tw, src, stderr); err != nil {
			f.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func archiveTree(tw *tar.Writer, src string, stderr io.Writer) error {
	base := filepath.Base(src)
	return filepath.Walk(src, func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		name := base
		if rel != "." {
			name = path.Join(base, filepath.ToSlash(rel))
		}
		link := ""
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		case !info.IsDir() && !info.Mode().IsRegular():
			fmt.Fprintf(stderr, "devvm: skipping %s: unsupported file type\n", p)
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		hdr.Format = tar.FormatPAX // long names and sub-second mtimes without warnings
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		r, err := os.Open(p)
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.Copy(tw, r)
		return err
	})
}

// archiveEntry is one top-level name in an archive plus every path beneath it,
// as read by readArchive.
type archiveEntry struct {
	name  string // top-level basename
	paths []archivePath
}

type archivePath struct {
	rel   string // path relative to the top-level entry ("" for the entry itself)
	isDir bool
}

// readArchive lists an archive's contents grouped by top-level entry, so a
// destination check can run before anything is extracted. Names must be
// relative and free of "..": the guest is the user's own box, but a download
// still never writes outside the chosen destination.
func readArchive(archive string) ([]archiveEntry, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []archiveEntry
	index := map[string]int{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		name, err := cleanArchiveName(hdr.Name)
		if err != nil {
			return nil, err
		}
		top, rel, _ := strings.Cut(name, "/")
		i, ok := index[top]
		if !ok {
			i = len(entries)
			index[top] = i
			entries = append(entries, archiveEntry{name: top})
		}
		entries[i].paths = append(entries[i].paths, archivePath{rel: rel, isDir: hdr.Typeflag == tar.TypeDir})
	}
}

func cleanArchiveName(name string) (string, error) {
	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if clean == "." || clean == "/" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("refusing archive entry %q", name)
	}
	return clean, nil
}

// extractArchive unpacks every entry whose top-level name is in roots, placing
// it at roots[name] (so a single entry can land under a new name). Existing
// files are replaced only when force is set; the caller has already checked
// for conflicts, so this is the second phase of an all-or-nothing copy.
func extractArchive(archive string, roots map[string]string, force bool) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, err := cleanArchiveName(hdr.Name)
		if err != nil {
			return err
		}
		top, rel, _ := strings.Cut(name, "/")
		root, ok := roots[top]
		if !ok {
			return fmt.Errorf("unexpected archive entry %q", hdr.Name)
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		mode := hdr.FileInfo().Mode().Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			// A directory that already exists is being merged into: leave its
			// mode alone, as cp -R would. New ones are created writable and get
			// the recorded mode once populated, so a read-only directory
			// doesn't block its own children.
			if _, err := os.Lstat(dst); err == nil {
				continue
			}
			if err := os.Mkdir(dst, mode|0700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{dst, mode})
		case tar.TypeReg:
			if force {
				os.Remove(dst) // a stale symlink or read-only file; errors surface on create
			}
			w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
			if err := os.Chmod(dst, mode); err != nil { // O_CREATE applies the umask
				return err
			}
			if err := os.Chtimes(dst, hdr.ModTime, hdr.ModTime); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if force {
				os.Remove(dst)
			}
			if err := os.Symlink(hdr.Linkname, dst); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type for %q", hdr.Name)
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- { // children before parents
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
	}
	return nil
}
