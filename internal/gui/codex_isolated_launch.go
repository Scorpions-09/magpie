package gui

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/settings"
)

func isolatedCodexHome() string {
	if runtime.GOOS == "windows" {
		return appdir.IsolatedCodexHome()
	}
	return ""
}

func openIsolatedCodex(home string) error {
	addr := settings.GatewayAddr()
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" || port == "" || served.Load() == nil {
		return errors.New("start this Magpie instance's local gateway before opening Codex")
	}
	a, err := agent.Find("codex")
	if err != nil {
		return err
	}
	bin := a.BinPath()
	if bin == "" {
		return errors.New("the Codex CLI is not available")
	}
	path := filepath.Join(home, "config.toml")
	if !strings.EqualFold(filepath.Clean(a.Path), path) {
		return errors.New("the detected Codex configuration is outside the isolated profile")
	}
	if err := a.Connect(); err != nil {
		return err
	}
	base := "http://" + addr + "/v1"
	table, err := edit.GetTOMLTable(path, "model_providers.magpie")
	if err != nil || table["base_url"] != base || table["wire_api"] != "responses" {
		return fmt.Errorf("Codex's Magpie provider is not connected to %s; reconnect Codex on the Agents page", base)
	}
	model, _ := edit.GetTOMLTop(path, "model")
	return agentTerminal(isolatedCodexCommand(home, bin, base, model), "Magpie Codex", settings.Shared().SessionTerminal)
}

// Use the same named Responses provider that Connect writes. Scope the
// selection and address to this new Codex process, including terminals
// that are started by an existing Windows Terminal process.
func isolatedCodexCommand(home, bin, base, model string) string {
	quote := func(value string) string {
		return "'" + strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛").Replace(value) + "'"
	}
	// TOML literal strings keep their quotes in native arguments even in
	// Windows PowerShell, which strips embedded double quotes.
	line := "$env:CODEX_HOME = " + quote(home) + "; & " + quote(bin) + " --no-daemon" +
		" -c " + quote("model_provider='magpie'") +
		" -c " + quote("model_providers.magpie.base_url='"+base+"'")
	if strings.HasPrefix(model, "gpt-") && !strings.Contains(model, "/") {
		line += " -m " + quote("codex/"+model)
	}
	return line
}
