package network

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// newSysfs returns a temp directory standing in for /sys/class/net. Tests
// populate it with per-interface subtrees via the helpers below and hand it
// to Discover/Classify through the injectable sysfs root.
func newSysfs(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// ifaceDir creates (if needed) and returns the fake sysfs entry for name.
func ifaceDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

// setType writes the ARPHRD link-layer type number file for name
// (1 = ether, 772 = loopback, 65534 = none).
func setType(t *testing.T, root, name string, arphrd int) {
	t.Helper()
	dir := ifaceDir(t, root, name)
	if err := os.WriteFile(filepath.Join(dir, "type"), []byte(strconv.Itoa(arphrd)+"\n"), 0o644); err != nil {
		t.Fatalf("write type: %v", err)
	}
}

// setUevent writes the uevent file for name.
func setUevent(t *testing.T, root, name, content string) {
	t.Helper()
	dir := ifaceDir(t, root, name)
	if err := os.WriteFile(filepath.Join(dir, "uevent"), []byte(content), 0o644); err != nil {
		t.Fatalf("write uevent: %v", err)
	}
}

// setWireless creates the wireless subdirectory that marks 802.11 devices.
func setWireless(t *testing.T, root, name string) {
	t.Helper()
	dir := ifaceDir(t, root, name)
	if err := os.MkdirAll(filepath.Join(dir, "wireless"), 0o755); err != nil {
		t.Fatalf("mkdir wireless: %v", err)
	}
}

// setDevice creates a real directory under the fake devices tree at relPath
// (slash-separated, e.g. "pci0000:00/0000:00:1f.6" or a path containing
// "usb1/..." for USB NICs), points <name>/device at it via symlink, and, when
// driver is non-empty, adds a device/driver symlink whose target basename is
// the driver name — mirroring the real sysfs layout Classify reads.
func setDevice(t *testing.T, root, name, relPath, driver string) {
	t.Helper()
	dir := ifaceDir(t, root, name)
	devDir := filepath.Join(root, "_devices", filepath.FromSlash(relPath))
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatalf("mkdir device dir: %v", err)
	}
	if err := os.Symlink(devDir, filepath.Join(dir, "device")); err != nil {
		t.Fatalf("symlink device: %v", err)
	}
	if driver != "" {
		drvDir := filepath.Join(root, "_drivers", driver)
		if err := os.MkdirAll(drvDir, 0o755); err != nil {
			t.Fatalf("mkdir driver dir: %v", err)
		}
		if err := os.Symlink(drvDir, filepath.Join(devDir, "driver")); err != nil {
			t.Fatalf("symlink driver: %v", err)
		}
	}
}
