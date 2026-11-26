//go:build !linux

package main

import (
	"errors"
	"net"
)

// peerUser has no implementation off Linux yet: the node refuses a
// connection it can't put a seat to, except in dev mode, where the peer is
// the invoking user.
func peerUser(*net.UnixConn) (string, error) {
	return "", errors.New("the bus node reads a socket's peer on Linux only, so far")
}
