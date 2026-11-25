//go:build !windows

package portable

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockFile(f *os.File) error         { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func replaceFile(from, to string) error { return os.Rename(from, to) }
