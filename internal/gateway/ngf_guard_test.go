package gateway

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ngfDir is the copy of NGF made by hack/sync-ngf.sh.
const ngfDir = "../third_party/ngf"

// deniedDirs are package names that must never be part of the copy: NGF's
// product telemetry, usage reporting and licensing, its agent and its
// provisioner.
var deniedDirs = map[string]bool{
	"telemetry":    true,
	"licensing":    true,
	"usage":        true,
	"productusage": true,
	"agent":        true,
	"provisioner":  true,
}

// deniedImports are import path fragments no copied file may import.
var deniedImports = []string{
	"github.com/nginx/telemetry-exporter",
	"github.com/nginx/agent",
	"/internal/third_party/ngf/controller/telemetry",
	"/internal/third_party/ngf/controller/licensing",
	"/internal/third_party/ngf/controller/provisioner",
	"/internal/third_party/ngf/controller/nginx/agent",
}

func readVersion(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join(ngfDir, "VERSION"))
	if err != nil {
		t.Fatalf("internal/third_party/ngf has no VERSION; run hack/sync-ngf.sh: %v", err)
	}
	defer f.Close()
	v := make(map[string]string)
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, " ") {
			continue
		}
		if k, val, ok := strings.Cut(line, ":"); ok {
			v[k] = strings.TrimSpace(val)
		}
	}
	return v
}

// TestNGFCopyIsUnmodified recomputes the checksum hack/sync-ngf.sh wrote
// into VERSION. A failure means a file below internal/third_party/ngf was
// edited by hand: put the change into hack/ngf-patches/ instead, or into
// internal/gateway, and re-run the sync.
func TestNGFCopyIsUnmodified(t *testing.T) {
	t.Parallel()
	want := readVersion(t)["tree-sha256"]
	if want == "" {
		t.Fatal("VERSION has no tree-sha256")
	}

	var files []string
	err := filepath.WalkDir(ngfDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(ngfDir, path)
		if err != nil {
			return err
		}
		if rel != "VERSION" {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)

	sum := sha256.New()
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(ngfDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(data)
		fmt.Fprintf(sum, "%s  %s\n", hex.EncodeToString(h[:]), rel)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		t.Errorf("internal/third_party/ngf differs from what hack/sync-ngf.sh copied (tree-sha256 %s, want %s)", got, want)
	}
}

// TestNGFCopyHasNoTelemetry fails if NGF's product telemetry, usage
// reporting, licensing, agent or provisioner made it into the copy -
// wxingress reports nothing to anyone.
func TestNGFCopyHasNoTelemetry(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	err := filepath.WalkDir(ngfDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if deniedDirs[d.Name()] {
				t.Errorf("denied package in the NGF copy: %s", path)
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range deniedImports {
				if strings.Contains(p, bad) {
					t.Errorf("%s imports %s", path, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
