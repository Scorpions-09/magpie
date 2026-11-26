package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestCodexWorkspaceHelperUsesOfficialWindowsPackage(t *testing.T) {
	const official = "OpenAI.Codex_26.1120.6357.0_x64__2p2nqsd0c76g0"
	dir := t.TempDir()
	bin := filepath.Join(dir, "app", "resources", "codex.exe")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oldList, oldRoot := windowsStorePackages, codexWindowsPackageRoot
	t.Cleanup(func() { windowsStorePackages, codexWindowsPackageRoot = oldList, oldRoot })
	windowsStorePackages = func() []string {
		return []string{"OpenAI.Codex_99.0.0.0_x64__wrong-publisher", official}
	}
	codexWindowsPackageRoot = func(pkg string) string {
		if pkg != official {
			t.Fatalf("read the package root for %q", pkg)
		}
		return dir
	}
	if got := provider.CodexWorkspaceAppExecutable(); got != bin {
		t.Fatalf("workspace helper = %q, want %q", got, bin)
	}
}

func TestCodexDesktopUsesPortableProfile(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CODEX_SQLITE_HOME", t.TempDir())
	dir := t.TempDir()
	home := filepath.Join(dir, "data", "codex")
	exe := filepath.Join(dir, "app", "ChatGPT.exe")
	cmd := codexAppProfileCommand(exe, home)
	if !slices.Equal(cmd.Args, []string{exe, "--user-data-dir=" + filepath.Join(dir, "data", "codex-desktop")}) {
		t.Fatalf("desktop command = %q", cmd.Args)
	}
	env := cmd.Environ()
	if !slices.Contains(env, "CODEX_HOME="+home) || !slices.Contains(env, "CODEX_SQLITE_HOME="+home) {
		t.Fatal("desktop command did not set its Codex and SQLite homes")
	}
}

// Open the Codex app (Yink88564391 on X): on a Mac it opens the bundle
// whose Info.plist says com.openai.codex, never the ChatGPT chat app's
// of the same name; on Windows it starts the Store package's app; with no
// app installed it opens nothing and says so.
func TestOpenCodexApp(t *testing.T) {
	restartSandbox(t)
	apps := filepath.Join(t.TempDir(), "Applications")
	appFolders = func() []string { return []string{apps} }
	var opened []string
	appOpen = func(_ context.Context, b string) error { opened = append(opened, b); return nil }
	var launched []winProc
	winLaunch = func(p winProc) error { launched = append(launched, p); return nil }

	restartOnMac = true
	if CodexAppInstalled() || OpenCodexApp(context.Background()) == nil || len(opened) != 0 {
		t.Fatalf("no app installed, yet installed=%v opened=%q", CodexAppInstalled(), opened)
	}
	installApp(t, apps, "ChatGPT.app", codexAppPlist("com.openai.chat", "1.2026.280"))
	if CodexAppInstalled() || OpenCodexApp(context.Background()) == nil || len(opened) != 0 {
		t.Fatalf("the chat app taken for Codex's: opened %q", opened)
	}
	installApp(t, apps, "Codex.app", codexAppPlist("com.openai.codex", "26.930.31730"))
	if !CodexAppInstalled() {
		t.Fatal("Codex.app not seen")
	}
	if err := OpenCodexApp(context.Background()); err != nil || len(opened) != 1 || opened[0] != filepath.Join(apps, "Codex.app") {
		t.Fatalf("opened %q (%v)", opened, err)
	}

	// Linux has no launcher magpie starts
	restartOnMac = false
	if CodexAppInstalled() || OpenCodexApp(context.Background()) == nil {
		t.Fatal("offered to open the app where magpie cannot")
	}
	if len(launched) != 0 {
		t.Fatalf("launched %v off Windows", launched)
	}
}
