//go:build !linux

package agent

import "errors"

// detachCleanup is only implemented on Linux; other platforms rely on the
// operator to remove the agent manually after it reports back.
func detachCleanup(name string, args ...string) error {
	return errors.New("automatic uninstall is only supported on linux")
}
