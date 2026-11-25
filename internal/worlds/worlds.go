// Package worlds keeps each generation's server directory and moves world
// files from one generation to the next: a full copy while the old server
// still plays, then only what changed while it's frozen.
package worlds

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Store is the directory holding every generation's server directory.
type Store struct {
	Root string
}

// Dir is generation gen's server directory.
func (s Store) Dir(gen int) string {
	return filepath.Join(s.Root, "gen-"+strconv.Itoa(gen))
}

// Generations lists the generations on disk, oldest first.
func (s Store) Generations() ([]int, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var gens []int
	for _, e := range entries {
		if n, ok := strings.CutPrefix(e.Name(), "gen-"); ok && e.IsDir() {
			if g, err := strconv.Atoi(n); err == nil {
				gens = append(gens, g)
			}
		}
	}
	slices.Sort(gens)
	return gens, nil
}

// Prune removes all but the newest keep generations, never removing active.
func (s Store) Prune(keep, active int) error {
	gens, err := s.Generations()
	if err != nil {
		return err
	}
	var errs []error
	for i, g := range gens {
		if g == active || i >= len(gens)-keep {
			continue
		}
		errs = append(errs, os.RemoveAll(s.Dir(g)))
	}
	return errors.Join(errs...)
}

// File is a file as it was when copied.
type File struct {
	Size    int64
	ModTime time.Time
}

func (f File) is(info fs.FileInfo) bool {
	return f.Size == info.Size() && f.ModTime.Equal(info.ModTime())
}

// changed marks a file that changed while it was copied: the next sync
// copies it again.
var changed = File{Size: -1}

// Manifest is what was copied, by path relative to the server directory.
type Manifest map[string]File

// skip is what's never copied: the lock a server holds on its world.
func skip(name string) bool { return name == "session.lock" }

// Copy copies the folders (relative paths) from src to dst, and says what it
// copied, so Sync can later move only what changed.
func Copy(src, dst string, folders []string) (Manifest, error) {
	m := Manifest{}
	err := walk(src, folders, func(rel string, info fs.FileInfo) error {
		return copyTo(src, dst, rel, info, m)
	})
	return m, err
}

// Sync brings dst up to date with src: it copies the files under folders that
// are new or changed in src since m, or that dst changed itself, and removes
// from dst those removed from src. m is updated to what dst now has, so syncs
// can follow one another. Files are replaced by rename, so a reader holding
// the old file open never sees it half-written.
func Sync(src, dst string, folders []string, m Manifest) (copied, removed int, err error) {
	seen := map[string]bool{}
	err = walk(src, folders, func(rel string, info fs.FileInfo) error {
		seen[rel] = true
		if was, ok := m[rel]; ok && was.is(info) && unchanged(filepath.Join(dst, rel), was) {
			return nil
		}
		copied++
		return copyTo(src, dst, rel, info, m)
	})
	if err != nil {
		return copied, removed, err
	}
	for rel := range m {
		if seen[rel] {
			continue
		}
		if err := os.Remove(filepath.Join(dst, rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return copied, removed, err
		}
		delete(m, rel)
		removed++
	}
	return copied, removed, nil
}

// unchanged says if the copy at path is still as it was copied: the new
// server may have saved over it.
func unchanged(path string, was File) bool {
	info, err := os.Stat(path)
	return err == nil && was.is(info)
}

// copyTo copies one file and notes it in m: as it was before copying, or as
// changed if it was written meanwhile.
func copyTo(src, dst, rel string, info fs.FileInfo, m Manifest) error {
	if err := copyFile(filepath.Join(src, rel), filepath.Join(dst, rel), info); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // removed since the walk saw it
		}
		return err
	}
	m[rel] = File{info.Size(), info.ModTime()}
	if after, err := os.Stat(filepath.Join(src, rel)); err != nil || !m[rel].is(after) {
		m[rel] = changed
	}
	return nil
}

func walk(root string, folders []string, f func(rel string, info fs.FileInfo) error) error {
	for _, folder := range folders {
		if !filepath.IsLocal(folder) {
			return fmt.Errorf("world folder %q isn't inside the server's directory", folder)
		}
		err := filepath.WalkDir(filepath.Join(root, folder), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil // removed while walking
				}
				return err
			}
			if d.IsDir() || skip(d.Name()) || !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			return f(rel, info)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, info fs.FileInfo) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".molt-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chtimes(tmp.Name(), info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// Prepare readies a new generation's directory: Paper there accepts players
// only through the proxy, forwarded with secret.
func Prepare(dir, secret string) error {
	conf := filepath.Join(dir, "config")
	if err := os.MkdirAll(conf, 0o750); err != nil {
		return err
	}
	// Paper fills in the rest of the file on start.
	global := fmt.Sprintf("proxies:\n  velocity:\n    enabled: true\n    online-mode: true\n    secret: %q\n", secret)
	return os.WriteFile(filepath.Join(conf, "paper-global.yml"), []byte(global), 0o600)
}
