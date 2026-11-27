//go:build darwin

package main

import (
	"net"
	"os/user"
	"strconv"

	"golang.org/x/sys/unix"
)

// peerUser is the Unix user at the other end of a unix socket, as the
// kernel saw it connect (getsockopt SOL_LOCAL, LOCAL_PEERCRED): darwin's
// SO_PEERCRED, and as hard to lie about.
func peerUser(c *net.UnixConn) (string, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return "", err
	}
	var cred *unix.Xucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return "", err
	}
	if cerr != nil {
		return "", cerr
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(cred.Uid), 10))
	if err != nil {
		return "", err
	}
	return u.Username, nil
}
