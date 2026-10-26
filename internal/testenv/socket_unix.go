//go:build !windows

package testenv

import (
	"fmt"
	"strconv"
	"sync"
	"syscall"
)

func bindClosedLoopback() (string, func() error, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return "", nil, fmt.Errorf("socket: %w", err)
	}
	syscall.CloseOnExec(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = syscall.Close(fd)
		return "", nil, fmt.Errorf("bind 127.0.0.1:0: %w", err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return "", nil, fmt.Errorf("getsockname: %w", err)
	}
	inet4, ok := sa.(*syscall.SockaddrInet4)
	if !ok || inet4.Port <= 0 {
		_ = syscall.Close(fd)
		return "", nil, fmt.Errorf("unexpected sockaddr %T", sa)
	}
	var once sync.Once
	var closeErr error
	closeFn := func() error {
		once.Do(func() {
			closeErr = syscall.Close(fd)
		})
		return closeErr
	}
	return strconv.Itoa(inet4.Port), closeFn, nil
}
