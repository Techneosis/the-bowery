//go:build linux

package main

import (
	"net"
	"os/user"
	"strconv"
	"syscall"
)

// peerUser is the Unix user at the other end of a unix socket, as the
// kernel saw it connect (SO_PEERCRED): what it can't lie about.
func peerUser(c *net.UnixConn) (string, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return "", err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return "", err
	}
	if cerr != nil {
		return "", cerr
	}
	u, err := user.LookupId(strconv.Itoa(int(cred.Uid)))
	if err != nil {
		return "", err
	}
	return u.Username, nil
}
