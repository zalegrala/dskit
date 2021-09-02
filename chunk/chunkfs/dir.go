package chunkfs

import (
	"fmt"
	"os"
)

// EnsureDirectory makes sure directory is there, creates it if necessary.
func EnsureDirectory(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, 0777)
		}
		return err
	}

	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}

	return nil
}
