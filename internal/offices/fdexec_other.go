//go:build !linux

package offices

import (
	"os"
	"os/exec"
)

// pinnedCommand off Linux runs the path that was hashed: there is no
// /proc/self/fd to execute the open file through. Root mode's parent
// checks are what keep the path from being swapped there.
func pinnedCommand(f *os.File) *exec.Cmd { return exec.Command(f.Name()) }
