package helps

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// AtomicWriteFile writes data to a same-directory temporary file, syncs it,
// and atomically replaces the destination. Callers retain the previous file if
// any write, sync, close, or replacement step fails.
func AtomicWriteFile(path string, data []byte, permission fs.FileMode) error {
	directory := filepath.Dir(path)
	temporary := filepath.Join(directory, ".tmp-"+uuid.NewString())
	file, errOpen := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, permission)
	if errOpen != nil {
		return fmt.Errorf("create atomic temporary file: %w", errOpen)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if _, errWrite := file.Write(data); errWrite != nil {
		_ = file.Close()
		return fmt.Errorf("write atomic temporary file: %w", errWrite)
	}
	if errSync := file.Sync(); errSync != nil {
		_ = file.Close()
		return fmt.Errorf("sync atomic temporary file: %w", errSync)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("close atomic temporary file: %w", errClose)
	}
	if errReplace := atomicReplaceFile(temporary, path); errReplace != nil {
		return fmt.Errorf("replace atomic destination: %w", errReplace)
	}
	removeTemporary = false
	return nil
}
