//go:build !windows

package helps

import (
	"errors"
	"os"
	"syscall"
)

func privateTaskOutputDirectory(path string) error {
	err := os.Mkdir(path, 0700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return os.ErrPermission
	}
	return nil
}

func regularPrivateTaskOutput(f *os.File) error {
	if err := regularTaskOutputPath(f); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return os.ErrPermission
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return os.ErrPermission
	}
	return nil
}
