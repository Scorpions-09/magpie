package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/gateway"
)

func TestIsolatedCodexOpenUsesDesktop(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the portable Codex profile launcher is for Windows")
	}
	sandboxHome(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, appdir.MarkerName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	appdir.UseExecutable(filepath.Join(dir, "magpie.exe"))
	t.Cleanup(func() { appdir.UseExecutable("") })
	home := filepath.Join(dir, appdir.DataName, "codex")
	t.Setenv("CODEX_HOME", home)
	prevInstalled, prevOpen := codexAppInstalled, codexAppProfileOpener
	prevGateway := served.Swap(nil)
	t.Cleanup(func() {
		codexAppInstalled, codexAppProfileOpener = prevInstalled, prevOpen
		served.Store(prevGateway)
	})
	codexAppInstalled = func() bool { return true }
	var opened string
	codexAppProfileOpener = func(profile string) error {
		opened = profile
		return nil
	}
	mux := http.NewServeMux()
	sessionRoutes(mux, nil)
	open := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/agents/open", strings.NewReader(`{"agent":"codex"}`)))
		return w
	}
	if w := open(); w.Code != http.StatusBadRequest || opened != "" {
		t.Fatalf("opened Codex without this instance's gateway: %d, %q, %s", w.Code, opened, w.Body)
	}
	served.Store(gateway.New())
	if w := open(); w.Code != http.StatusNoContent || opened != home {
		t.Fatalf("did not open the official desktop on %q: %d, %q, %s", home, w.Code, opened, w.Body)
	}
}

func TestIsolatedCodexLaunchCommand(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the isolated launcher uses Windows PowerShell")
	}
	home := filepath.Join(t.TempDir(), "Local Codex's profile")
	script := filepath.Join(t.TempDir(), "Local Codex's CLI.ps1")
	if err := os.WriteFile(script, []byte(`@($env:CODEX_HOME) + $args | ConvertTo-Json -Compress`), 0o600); err != nil {
		t.Fatal(err)
	}
	const base = "http://127.0.0.1:13426/v1"
	for _, model := range []string{"gpt-5.5", "codex/gpt-5.5"} {
		t.Run(model, func(t *testing.T) {
			line := isolatedCodexCommand(home, script, base, model)
			cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-EncodedCommand", psEncode(line))
			out, err := cmd.Output()
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					t.Fatalf("run launch command: %v: %s", err, exit.Stderr)
				}
				t.Fatal(err)
			}
			var got []string
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("read CLI arguments: %v: %s", err, out)
			}
			want := []string{home, "--no-daemon", "-c", "model_provider='magpie'", "-c", "model_providers.magpie.base_url='" + base + "'"}
			if model == "gpt-5.5" {
				want = append(want, "-m", "codex/gpt-5.5")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Codex launch arguments = %q, want %q", got, want)
			}
		})
	}
}
