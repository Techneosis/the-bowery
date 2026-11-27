//go:build linux || darwin

package main

import (
	"net"
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// The bus node puts a seat to a local client by the socket's peer: on every
// platform the node runs on in root mode, that has to be the user who
// connected (BUS-DARWIN-PEERCRED: darwin closed every connection).
func TestPeerUserIsTheConnectingUser(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	// Short: darwin's TempDir is longer than a socket path may be.
	dir, err := os.MkdirTemp("/tmp", "peercred")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "node.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	c, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	got, err := peerUser(c)
	if err != nil {
		t.Fatalf("peerUser: %v", err)
	}
	if got != me.Username {
		t.Fatalf("peerUser = %q, want %q", got, me.Username)
	}
}
