//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
)

// Windows has no chmod 600: the mode bits only toggle read-only. What
// restricts a file to its owner there is its ACL, so the fallback file is
// created empty, its inherited permissions are removed with icacls, and only
// the current user is granted access, all before any token is written.
// If icacls fails nothing is written, so the credential is never left in a
// file the ACL has not been restricted on.
func createPrivate(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := restrictToUser(path); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return f, nil
}

func restrictToUser(path string) error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("cannot find the current user to restrict %s: %w", path, err)
	}
	// /inheritance:r drops the parent folder's entries; /grant:r replaces
	// any entry for this user with full control.
	if out, err := exec.Command("icacls", path, "/inheritance:r", "/grant:r", u.Username+":F").CombinedOutput(); err != nil {
		return fmt.Errorf("icacls could not restrict %s to %s: %v: %s", path, u.Username, err, out)
	}
	return nil
}

// checkPrivate has nothing to verify cheaply on Windows; the file was
// restricted when it was created.
func checkPrivate(path string) error {
	_, err := os.Stat(path)
	return err
}
