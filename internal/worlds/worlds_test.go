package worlds

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func TestCopyThenSync(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "world/region/r.0.0.mca"), "a")
	write(t, filepath.Join(src, "world/region/r.1.0.mca"), "b")
	write(t, filepath.Join(src, "world/playerdata/p.dat"), "p")
	write(t, filepath.Join(src, "world/session.lock"), "lock")
	write(t, filepath.Join(src, "plugins/old.jar"), "not a world")
	write(t, filepath.Join(src, "ops.json"), "[]")

	m, err := Copy(src, dst, []string{"world", "ops.json", "whitelist.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 4 || read(t, filepath.Join(dst, "world/region/r.0.0.mca")) != "a" || read(t, filepath.Join(dst, "ops.json")) != "[]" {
		t.Fatalf("copied %v", m)
	}
	for _, p := range []string{"world/session.lock", "plugins/old.jar"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err == nil {
			t.Errorf("copied %s", p)
		}
	}

	// The old server's changes come over, and so does its version of a file
	// the new server saved over.
	write(t, filepath.Join(dst, "world/region/r.1.0.mca"), "b, as the new server saved it")
	later := time.Now().Add(time.Minute)
	write(t, filepath.Join(src, "world/region/r.0.0.mca"), "a2")
	_ = os.Chtimes(filepath.Join(src, "world/region/r.0.0.mca"), later, later)
	write(t, filepath.Join(src, "world/region/r.2.0.mca"), "c")
	_ = os.Remove(filepath.Join(src, "world/playerdata/p.dat"))

	copied, removed, err := Sync(src, dst, []string{"world", "ops.json", "whitelist.json"}, m)
	if err != nil {
		t.Fatal(err)
	}
	if copied != 3 || removed != 1 {
		t.Errorf("copied %d, removed %d", copied, removed)
	}
	for p, want := range map[string]string{
		"world/region/r.0.0.mca": "a2",
		"world/region/r.1.0.mca": "b",
		"world/region/r.2.0.mca": "c",
	} {
		if got := read(t, filepath.Join(dst, p)); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "world/playerdata/p.dat")); err == nil {
		t.Error("kept a removed file")
	}
	// The copy keeps the time, so the next sync doesn't copy it again.
	if fi, _ := os.Stat(filepath.Join(dst, "world/region/r.0.0.mca")); fi.ModTime().Unix() != later.Unix() {
		t.Errorf("mtime %v, want %v", fi.ModTime(), later)
	}
	if copied, removed, err := Sync(src, dst, []string{"world", "ops.json", "whitelist.json"}, m); err != nil || copied+removed != 0 {
		t.Errorf("a second sync copied %d, removed %d: %v", copied, removed, err)
	}
}

func TestFoldersStayInside(t *testing.T) {
	if _, err := Copy(t.TempDir(), t.TempDir(), []string{"../etc"}); err == nil {
		t.Error("copied from outside the server's directory")
	}
}

func TestPrune(t *testing.T) {
	s := Store{Root: t.TempDir()}
	for _, g := range []int{1, 2, 3, 4, 10} {
		if err := os.MkdirAll(s.Dir(g), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prune(2, 2); err != nil {
		t.Fatal(err)
	}
	if gens, _ := s.Generations(); !slices.Equal(gens, []int{2, 4, 10}) {
		t.Errorf("left %v", gens)
	}
}
