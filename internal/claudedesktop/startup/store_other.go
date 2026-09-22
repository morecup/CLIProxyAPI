//go:build !windows

package startup

import "os"

func replaceIdentityFile(source, destination string) error {
	return os.Rename(source, destination)
}
