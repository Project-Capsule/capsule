//go:build !linux

package boot

// DetectInstallerMode is a no-op on non-Linux. Installer detection
// requires sysfs / kernel cmdline parsing that's Linux-specific.
func DetectInstallerMode() (bool, []TargetDisk) {
	return false, nil
}
