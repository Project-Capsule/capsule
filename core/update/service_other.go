//go:build !linux

package update

import "errors"

func defaultReboot() error {
	return errors.New("update: reboot not implemented on non-Linux")
}
