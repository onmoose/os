//go:build !linux

package usermgr

import "errors"

// PrepareFolder implements hostagent.UserManager. The real op walks the home
// with Linux-only system calls (folder_linux.go). host-agent-real only runs on
// Linux; this stub exists so the package still builds elsewhere.
func (m *LinuxUserManager) PrepareFolder(username, rel string) (bool, error) {
	return false, errors.New("usermgr: prepare-folder is only supported on Linux")
}

// ListFolders implements hostagent.UserManager. Linux only, like PrepareFolder.
func (m *LinuxUserManager) ListFolders(username, rel string) ([]string, bool, error) {
	return nil, false, errors.New("usermgr: list-folders is only supported on Linux")
}

// PinFolder implements hostagent.UserManager. The real op walks and mounts with
// Linux-only system calls (folder_linux.go).
func (m *LinuxUserManager) PinFolder(username string, shared bool, rel string) (string, error) {
	return "", errors.New("usermgr: pin-folder is only supported on Linux")
}

// RestoreFolderPins is the boot restorer. The real op walks and mounts with
// Linux-only system calls (folder_linux.go).
func RestoreFolderPins(stateDir, pinRoot string) error {
	return errors.New("usermgr: restore-folder-pins is only supported on Linux")
}
