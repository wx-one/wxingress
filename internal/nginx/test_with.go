package nginx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TestTimeout is the longest nginx -t of TestWithConfigs runs: the API
// server gives up on a webhook after 10 seconds by default, and an nginx -t
// that hangs - a resolver that does not answer, a file that blocks - must
// not keep a check, and the process, alive.
const TestTimeout = 10 * time.Second

// TestWithConfigs runs nginx -t of the configuration as it is with the
// files of conf.d named (without .conf) replaced by, or added as, the
// contents given. Its errors are nginx's.
func (lm *LocalManager) TestWithConfigs(replaced map[string][]byte) error {
	test, err := lm.PrepareTestWithConfigs(replaced)
	if err != nil {
		return err
	}
	return test(context.Background())
}

// PrepareTestWithConfigs copies the configuration as it is now - the main
// config and the files of conf.d, those named (without .conf) replaced by,
// or added as, the contents given - beside the real one, and returns the
// nginx -t of that copy, which removes it when done. Nothing nginx runs with
// is changed. The copy is made at once, so the test can run later, while the
// configuration changes, and sees it as it was: the caller prepares under
// the lock of the configuration and tests without it. The test takes at most
// TestTimeout.
func (lm *LocalManager) PrepareTestWithConfigs(replaced map[string][]byte) (func(context.Context) error, error) {
	main, err := os.ReadFile(lm.mainConfFilename)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", lm.mainConfFilename, err)
	}
	include := "include " + filepath.Join(lm.confdPath, "*.conf") + ";"
	if !bytes.Contains(main, []byte(include)) {
		return nil, fmt.Errorf("%s does not include %s", lm.mainConfFilename, filepath.Join(lm.confdPath, "*.conf"))
	}
	for name := range replaced {
		// the names come from objectMetaToFileName: a namespace and a name, no path
		if strings.ContainsAny(name, "/\\") {
			return nil, fmt.Errorf("invalid config name %q", name)
		}
	}

	dir, err := os.MkdirTemp(filepath.Dir(lm.mainConfFilename), ".admission-")
	if err != nil {
		return nil, err
	}
	confd := filepath.Join(dir, "conf.d")
	conf := filepath.Join(dir, "nginx.conf")
	if err := lm.copyConfigForTest(dir, confd, conf, main, include, replaced); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}

	return func(ctx context.Context) error {
		defer os.RemoveAll(dir)
		ctx, cancel := context.WithTimeout(ctx, TestTimeout)
		defer cancel()
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, getBinaryFileName(lm.debug), "-t", "-q", "-c", conf) // #nosec G204
		cmd.Stderr = &stderr
		// what nginx started and left behind holding stderr does not keep the test
		cmd.WaitDelay = time.Second
		err := cmd.Run()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("nginx configuration test did not finish within %s", TestTimeout)
		}
		if err != nil {
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
	}, nil
}

// copyConfigForTest makes the copy of the configuration of PrepareTestWithConfigs in dir.
func (lm *LocalManager) copyConfigForTest(dir, confd, conf string, main []byte, include string, replaced map[string][]byte) error {
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

	// the files of conf.d are copied, not linked: a sync writes them in place while the test runs
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
		if err := copyFile(filepath.Join(lm.confdPath, e.Name()), filepath.Join(confd, e.Name())); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// deleted since it was listed
				continue
			}
			return err
		}
	}
	for name, content := range replaced {
		if err := os.WriteFile(filepath.Join(confd, name+".conf"), content, 0o640); err != nil {
			return err
		}
	}
	return os.WriteFile(conf, bytes.Replace(main, []byte(include), []byte("include "+filepath.Join(confd, "*.conf")+";"), 1), 0o640)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
