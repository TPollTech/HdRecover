//go:build !windows

package recovery

import "os"

func openSource(path string) (*os.File, error) {
	return os.Open(path)
}
