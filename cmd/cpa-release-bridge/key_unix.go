//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

func privateKeyFile(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != 0 {
		return errors.New("control key must be a root-owned regular file with no group or other permissions")
	}
	return nil
}
