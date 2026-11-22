package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The image has no shell and no coreutils, so the two things the chart runs
// in it besides the controller are subcommands of the controller binary:
//
//	nginx-ingress sleep <seconds>       the preStop hook where the cluster has
//	                                    no sleep action (before Kubernetes 1.30)
//	nginx-ingress copy <source> <dest>  the init container that fills the
//	                                    emptyDir for /etc/nginx when the root
//	                                    file system is read-only
//
// runSubcommand runs one of them and exits when os.Args names one, before any
// flag is parsed; otherwise it returns.
func runSubcommand(args []string) {
	if len(args) < 2 {
		return
	}
	var err error
	switch args[1] {
	case "sleep":
		err = sleepCommand(args[2:])
	case "copy":
		err = copyCommand(args[2:])
	default:
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", args[1], err)
		os.Exit(1)
	}
	os.Exit(0)
}

func sleepCommand(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: sleep <seconds>")
	}
	seconds, err := strconv.ParseFloat(args[0], 64)
	if err != nil || seconds < 0 || seconds > 3600 {
		return fmt.Errorf("invalid number of seconds %q", args[0])
	}
	time.Sleep(time.Duration(seconds * float64(time.Second)))
	return nil
}

// copyCommand copies what is inside source into dest, like cp -dR source/. dest:
// directories, regular files with their permission bits, and symbolic links
// as links.
func copyCommand(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: copy <source> <dest>")
	}
	src, dst := filepath.Clean(args[0]), filepath.Clean(args[1])
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir() && rel == ".":
			// dest itself, a mount point that need not be ours
			return os.MkdirAll(target, 0o755)
		case d.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()|0o700); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		default:
			return nil
		}
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src) // #nosec G304 -- a path the pod spec gives
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode|0o600) // #nosec G304
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode|0o600)
}
