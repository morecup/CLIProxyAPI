//go:build !windows

package helps

import "os"

func atomicReplaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
