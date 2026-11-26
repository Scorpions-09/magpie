//go:build !windows

package agent

func installedWindowsStorePackages() []string { return nil }

func windowsStorePackageRoot(string) string { return "" }

func registeredWindowsApp(string) (dir, version string) { return "", "" }
