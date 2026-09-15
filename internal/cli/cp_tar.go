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
	"syscall"
)

// writeArchive tars each source (a file, symlink or directory tree) under its
// basename. Symlinks are stored as links, never followed; modes and mtimes are
// kept; ownership and xattrs are deliberately not (the guest extracts as its
// own user, and macOS quarantine metadata must not travel). Entries of other
// types (sockets, devices) are skipped with a note, as tar would.
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

// readArchive lists an archive's contents grouped by top-level entry and
// validates every entry, so a destination check can run — and any refusal
// happen — before anything is extracted. Names must be relative and free of
// "..", no path may appear twice, a hard link must point inside its own
// top-level entry, and entry types neither side can represent (fifos, devices)
// are skipped with a note, as the write side does. The guest is the user's
// own box, but a download still never writes outside the chosen destination:
// extractArchive refuses to write through a symlink, which is the only way a
// well-formed name could escape.
func readArchive(archive string, stderr io.Writer) ([]archiveEntry, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []archiveEntry
	index := map[string]int{}
	seen := map[string]bool{}  // accepted entries, for hard-link targets
	named := map[string]bool{} // every entry, accepted or skipped
	isDir := map[string]bool{}
	isLink := map[string]bool{} // accepted directories: never valid hard-link targets
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, err := cleanArchiveName(hdr.Name)
		if err != nil {
			return nil, err
		}
		// One path, one entry. An honest tar never names a path twice; a
		// crafted one could warm extractArchive's symlink-free cache with a
		// harmless entry and then swap the directory for a symlink under -f.
		if named[name] {
			return nil, fmt.Errorf("refusing archive entry %q: named twice", hdr.Name)
		}
		named[name] = true
		top, rel, _ := strings.Cut(name, "/")
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		case tar.TypeLink:
			target, err := cleanArchiveName(hdr.Linkname)
			if err != nil {
				return nil, fmt.Errorf("hard link %q: %w", hdr.Name, err)
			}
			if t, _, _ := strings.Cut(target, "/"); t != top || !seen[target] {
				return nil, fmt.Errorf("refusing hard link %q -> %q: target is outside the copied tree", hdr.Name, hdr.Linkname)
			}
			if isDir[target] {
				// link(2) refuses directories; the copy fallback would then
				// fail mid-extraction, breaking all-or-nothing.
				return nil, fmt.Errorf("refusing hard link %q -> %q: target is a directory", hdr.Name, hdr.Linkname)
			}
			if isLink[target] {
				// Whether link(2) follows a symlink target is platform
				// folklore (Linux doesn't; darwin's has not been verified),
				// and a hard link to what a symlink points at would alias a
				// file outside the tree. An honest tar never emits this.
				return nil, fmt.Errorf("refusing hard link %q -> %q: target is a symlink", hdr.Name, hdr.Linkname)
			}
		default:
			fmt.Fprintf(stderr, "devvm: skipping %s: unsupported file type\n", hdr.Name)
			continue
		}
		seen[name] = true
		if hdr.Typeflag == tar.TypeDir {
			isDir[name] = true
		}
		if hdr.Typeflag == tar.TypeSymlink {
			isLink[name] = true
		}
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
// for conflicts via readArchive, so this is the second phase of an
// all-or-nothing copy and skips what readArchive skipped.
//
// Nothing is ever written through a symlink: every directory component below
// a root is Lstat'ed before use. A tar from an honest tree never has children
// under a symlinked directory (tar stores the link and does not descend), so
// this only ever refuses a crafted archive, e.g. `proj/link -> /etc` followed
// by `proj/link/passwd`.
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
	safeDirs := map[string]bool{} // directories verified symlink-free
	// forget drops a path and everything under it from the cache: once
	// force has removed something, whatever was verified there is gone.
	forget := func(p string) {
		for k := range safeDirs {
			if k == p || strings.HasPrefix(k, p+string(filepath.Separator)) {
				delete(safeDirs, k)
			}
		}
	}
	remove := func(p string) {
		os.Remove(p) // a stale symlink or read-only file; errors surface on create
		forget(p)
	}
	safeParent := func(root, rel string) error {
		if rel == "" {
			return nil // the top-level entry itself; nothing below root yet
		}
		// root itself first: with -f a pre-existing DEST/<name> symlink would
		// otherwise be followed by every entry written beneath it.
		if !safeDirs[root] {
			info, err := os.Lstat(root)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing to write through symlink %s", root)
			}
			safeDirs[root] = true
		}
		dir := path.Dir(rel)
		if dir == "." {
			return nil
		}
		parts := strings.Split(dir, "/")
		for i := range parts {
			sub := filepath.Join(root, filepath.FromSlash(path.Join(parts[:i+1]...)))
			if safeDirs[sub] {
				continue
			}
			info, err := os.Lstat(sub)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing to write through symlink %s", sub)
			}
			safeDirs[sub] = true
		}
		return nil
	}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
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
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
		default:
			continue // readArchive already noted the skip; nothing to verify
		}
		if err := safeParent(root, rel); err != nil {
			return err
		}
		mode := hdr.FileInfo().Mode().Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			// A directory that already exists is being merged into: leave its
			// mode alone, as cp -R would. New ones are created writable and get
			// the recorded mode once populated, so a read-only directory
			// doesn't block its own children.
			if info, err := os.Lstat(dst); err == nil {
				if info.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("refusing to write through symlink %s", dst)
				}
				continue
			}
			if err := os.Mkdir(dst, mode|0700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{dst, mode})
		case tar.TypeReg:
			if force {
				remove(dst)
			}
			w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
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
				remove(dst)
			}
			if err := os.Symlink(hdr.Linkname, dst); err != nil {
				return err
			}
		case tar.TypeLink:
			// readArchive verified the target sits inside this entry's tree
			// and precedes this header, so it has already been extracted.
			target, _ := cleanArchiveName(hdr.Linkname)
			_, targetRel, _ := strings.Cut(target, "/")
			src := filepath.Join(root, filepath.FromSlash(targetRel))
			if force {
				remove(dst)
			}
			if err := os.Link(src, dst); err != nil {
				// Some filesystems refuse hard links; a copy keeps the content.
				if err := copyFile(src, dst, mode); err != nil {
					return err
				}
			}
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- { // children before parents
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
	}
	return nil
}

// copyFile is the hard-link fallback: a plain copy with the recorded mode. A
// hard link to a symlink is allowed (it stays inside the tree), but a copy of
// it must reproduce the link, not the content of whatever it points at.
func copyFile(src, dst string, mode fs.FileMode) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	r, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}
