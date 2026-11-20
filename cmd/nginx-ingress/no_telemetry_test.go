package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// wxingress has no product telemetry, and none may come back with a merge of
// NGINX Ingress Controller (docs/developer/upstream.md). Tracing the user
// configures - nginx's otel module sending to the user's own collector - is
// configuration of nginx, not code of the controller, and is not touched here.

// telemetryImports are import path fragments no package of the repository
// may import: NIC's and NGF's telemetry, the exporter they report with, the
// OTLP exporters that ship it and NGINX Agent.
var telemetryImports = []string{
	"github.com/wx-one/wxingress/internal/telemetry",
	"github.com/nginx/telemetry-exporter",
	"go.opentelemetry.io/otel/exporters/",
	"github.com/nginx/agent",
}

const repoRoot = "../.."

// TestNoTelemetryPackages fails on a directory internal/telemetry and on any
// Go file of the repository (tests included, the NGF copy included) that
// imports one of telemetryImports.
func TestNoTelemetryPackages(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(filepath.Join(repoRoot, "internal", "telemetry")); err == nil {
		t.Error("internal/telemetry exists: NIC's telemetry is removed from wxingress, delete it again (docs/developer/upstream.md)")
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// hidden directories hold no Go of ours, but CI's module cache
			// ($CI_PROJECT_DIR/.go) and build cache (.cache) lie in the checkout
			if path != repoRoot && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			switch d.Name() {
			case ".git", "node_modules", "build", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			// a file meta or a generator writes that is not Go of ours
			return nil //nolint:nilerr
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range telemetryImports {
				if strings.HasPrefix(p, bad) {
					t.Errorf("%s imports %s: wxingress has no telemetry", path, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNoTelemetryInTheBinary asks go list for every package the controller
// links: none may be one of telemetryImports, whatever brought it in.
func TestNoTelemetryInTheBinary(t *testing.T) {
	t.Parallel()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go binary to list the packages with")
	}
	out, err := exec.Command(goBin, "list", "-deps", ".").Output() //nolint:gosec // a fixed command
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, p := range strings.Fields(string(out)) {
		for _, bad := range telemetryImports {
			if strings.HasPrefix(p, bad) {
				t.Errorf("the controller links %s: wxingress has no telemetry", p)
			}
		}
	}
}
