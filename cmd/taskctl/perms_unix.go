//go:build !windows

package main

import (
	"fmt"
	"os"
)

// createPrivate creates (or truncates) a file only the owner can read.
func createPrivate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
}

// checkPrivate refuses a file other users can read, as ssh does for keys.
func checkPrivate(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by other users (mode %04o); run: chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	return nil
}
