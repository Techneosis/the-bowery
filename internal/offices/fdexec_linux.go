//go:build linux

package offices

import (
	"os"
	"os/exec"
)

// pinnedCommand runs the opened, hashed file itself: it is handed to the
// child as fd 3 and executed as /proc/self/fd/3, so a file swapped at the
// path after hashing is never what runs. (An interpreter script reopens
// the same fd through that path, which is why it isn't close-on-exec.)
func pinnedCommand(f *os.File) *exec.Cmd {
	cmd := exec.Command("/proc/self/fd/3")
	cmd.ExtraFiles = []*os.File{f}
	return cmd
}
