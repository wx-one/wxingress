package nginx

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// At startup the Ingress configs named <namespace>-<name> go, and nothing else in conf.d.
func TestRemoveLegacyIngressConfigs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"default-cafe.conf":         "# configuration for default/cafe\nserver {}\n",
		"a-b-c.conf":                "# configuration for a-b/c\nserver {}\n",
		"default_cafe.conf":         "# configuration for default/cafe\nserver {}\n",
		"vs_default_cafe.conf":      "server {}\n",
		"my-extra.conf":             "server {}\n",
		"mismatch-name.conf":        "# configuration for default/other\n",
		"_default-server.conf":      "# configuration for default/x\n",
		"default-cafe.conf.disable": "# configuration for default/cafe\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removeLegacyIngressConfigs(slog.New(slog.DiscardHandler), dir)
	for name := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		gone := os.IsNotExist(err)
		if want := name == "default-cafe.conf" || name == "a-b-c.conf"; gone != want {
			t.Errorf("%s removed %v, want %v", name, gone, want)
		}
	}
}
