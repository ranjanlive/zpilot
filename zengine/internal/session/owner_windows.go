//go:build windows

package session

import (
	"errors"
	"syscall"
)

type ownerLock struct{ h syscall.Handle }

func acquireOwner(path string) (*ownerLock, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return &ownerLock{h: h}, nil
}
func ownerBusy(err error) bool {
	return errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33))
}
func (l *ownerLock) close() { _ = syscall.CloseHandle(l.h) }
