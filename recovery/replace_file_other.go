//go:build !windows

package recovery

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
