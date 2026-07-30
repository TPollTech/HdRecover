//go:build windows

package recovery

import (
	"os"
	"syscall"
)

const fileFlagSequentialScan = 0x08000000

func openSource(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		name,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|fileFlagSequentialScan,
		0,
	)
	if err != nil {
		// Alguns adaptadores USB ignoram ou rejeitam dicas de cache. Neles,
		// repetimos a abertura normal para preservar a compatibilidade.
		handle, err = syscall.CreateFile(
			name,
			syscall.GENERIC_READ,
			syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
			nil,
			syscall.OPEN_EXISTING,
			syscall.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err != nil {
			return nil, err
		}
	}
	return os.NewFile(uintptr(handle), path), nil
}
