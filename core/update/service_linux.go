//go:build linux

package update

import (
	"log/slog"

	"golang.org/x/sys/unix"
)

func defaultReboot() error {
	unix.Sync()
	slog.Info("rebooting")
	return unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
}
