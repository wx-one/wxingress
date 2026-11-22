package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSleepCommand(t *testing.T) {
	t.Parallel()
	start := time.Now()
	if err := sleepCommand([]string{"0.05"}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("returned before the time given")
	}
	for _, bad := range [][]string{{}, {"x"}, {"-1"}, {"1", "2"}} {
		if err := sleepCommand(bad); err == nil {
			t.Errorf("sleepCommand(%q) took it", bad)
		}
	}
}

func TestCopyCommand(t *testing.T) {
	t.Parallel()
	src, dst := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "njs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "mime.types"), []byte("types {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "njs", "a.js"), []byte("a"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/lib/nginx/modules", filepath.Join(src, "modules")); err != nil {
		t.Fatal(err)
	}
	if err := copyCommand([]string{src, dst}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "mime.types")); err != nil || string(b) != "types {}" {
		t.Errorf("mime.types: %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "njs", "a.js")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("njs/a.js: %v, %v", fi, err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "modules")); err != nil || l != "/usr/lib/nginx/modules" {
		t.Errorf("modules: %q, %v", l, err)
	}
}
