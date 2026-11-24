//go:build !windows

package session

import (
	"errors"
	"os"
	"syscall"
)

type ownerLock struct{ f *os.File }

func acquireOwner(path string) (*ownerLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return &ownerLock{f: f}, nil
}
func ownerBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
func (l *ownerLock) close() { _ = l.f.Close() }
