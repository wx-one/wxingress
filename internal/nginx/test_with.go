package nginx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// TestWithConfigs runs nginx -t of the configuration as it is with the
// files of conf.d named (without .conf) replaced by, or added as, the
// contents given: a main config beside the real one includes a directory of
// links to the files of conf.d but those, and the new contents. Nothing nginx
// runs with is changed. Its errors are nginx's.
func (lm *LocalManager) TestWithConfigs(replaced map[string][]byte) error {
	main, err := os.ReadFile(lm.mainConfFilename)
	if err != nil {
		return fmt.Errorf("reading %s: %w", lm.mainConfFilename, err)
	}
	include := "include " + filepath.Join(lm.confdPath, "*.conf") + ";"
	if !bytes.Contains(main, []byte(include)) {
		return fmt.Errorf("%s does not include %s", lm.mainConfFilename, filepath.Join(lm.confdPath, "*.conf"))
	}

	dir, err := os.MkdirTemp(filepath.Dir(lm.mainConfFilename), ".admission-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	confd := filepath.Join(dir, "conf.d")
	if err := os.Mkdir(confd, 0o750); err != nil {
		return err
	}

	// what the configuration includes relative to its own directory (oidc/, mime.types) is found beside it
	base := filepath.Dir(lm.mainConfFilename)
	top, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	for _, e := range top {
		if e.Name() == filepath.Base(lm.mainConfFilename) || e.Name() == filepath.Base(lm.confdPath) || strings.HasPrefix(e.Name(), ".admission-") {
			continue
		}
		if err := os.Symlink(filepath.Join(base, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}

	entries, err := os.ReadDir(lm.confdPath)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".conf")
		if !ok || e.IsDir() {
			continue
		}
		if _, isReplaced := replaced[name]; isReplaced {
			continue
		}
		if err := os.Symlink(filepath.Join(lm.confdPath, e.Name()), filepath.Join(confd, e.Name())); err != nil {
			return err
		}
	}
	for name, content := range replaced {
		// the names come from objectMetaToFileName: a namespace and a name, no path
		if strings.ContainsAny(name, "/\\") {
			return fmt.Errorf("invalid config name %q", name)
		}
		if err := os.WriteFile(filepath.Join(confd, name+".conf"), content, 0o640); err != nil {
			return err
		}
	}
	conf := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(conf, bytes.Replace(main, []byte(include), []byte("include "+filepath.Join(confd, "*.conf")+";"), 1), 0o640); err != nil {
		return err
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(context.Background(), getBinaryFileName(lm.debug), "-t", "-q", "-c", conf) // #nosec G204
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var lines []string
		for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || (strings.HasPrefix(line, "nginx: configuration file") && strings.HasSuffix(line, "test failed")) {
				continue
			}
			// the file as the config will be, not the one tested
			lines = append(lines, strings.ReplaceAll(line, confd, lm.confdPath))
		}
		if len(lines) == 0 {
			return fmt.Errorf("nginx configuration test failed: %w", err)
		}
		return fmt.Errorf("%s", strings.Join(lines, "; "))
	}
	return nil
}
