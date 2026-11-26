package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/proc"
)

// CodexWorkspace is a membership shown in the sign-in account picker.
type CodexWorkspace struct {
	ID      string `json:"accountId"`
	Name    string `json:"name"`
	Current bool   `json:"current,omitempty"`
	Status  string `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
}

type codexWorkspaceServer struct {
	authFile string
	in       *json.Encoder
	out      *json.Decoder
	nextID   int
}

type codexWorkspaceSessions struct {
	Active string                  `json:"activeSessionId"`
	Items  []codexWorkspaceSession `json:"sessions"`
}

type codexWorkspaceSession struct {
	ID         string           `json:"sessionId"`
	UserID     string           `json:"userId"`
	Current    string           `json:"selectedWorkspaceAccountId"`
	Workspaces []CodexWorkspace `json:"workspaces"`
}

func (s *codexWorkspaceServer) call(method string, params, result any) error {
	s.nextID++
	if err := s.in.Encode(map[string]any{"id": s.nextID, "method": method, "params": params}); err != nil {
		return errors.New("could not send a request to Codex")
	}
	for {
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := s.out.Decode(&reply); err != nil {
			return errors.New("Codex did not answer the workspace request")
		}
		if reply.ID != s.nextID {
			continue // account/routing notifications can arrive before the reply
		}
		if reply.Error != nil {
			if reply.Error.Code == -32601 {
				return errors.New("this version of Codex does not support workspace discovery")
			}
			return fmt.Errorf("Codex: %s", reply.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(reply.Result, result)
		}
		return nil
	}
}

func (s *codexWorkspaceServer) list(refresh bool) (codexWorkspaceSession, error) {
	var out codexWorkspaceSessions
	if err := s.call("accountSession/list", map[string]bool{"refreshWorkspaceMetadata": refresh}, &out); err != nil {
		return codexWorkspaceSession{}, err
	}
	for _, session := range out.Items {
		if session.ID != "" && session.ID == out.Active {
			return session, nil
		}
	}
	return codexWorkspaceSession{}, errors.New("Codex did not find the ChatGPT sign-in")
}

func (s *codexWorkspaceServer) selectWorkspace(session, workspace string) error {
	return s.call("accountSession/switch", map[string]any{
		"sessionId": session, "accountId": workspace, "allowBrowserLogin": false,
	}, nil)
}

func (s *codexWorkspaceServer) auth() (json.RawMessage, error) {
	return os.ReadFile(s.authFile)
}

// CodexWorkspaceAppExecutable locates the helper shipped with the Windows
// Codex app when no standalone Codex executable is available.
var CodexWorkspaceAppExecutable = func() string { return "" }

func codexWorkspaceExecutable() string {
	if exe := codexExecutable(); exe != "" {
		return exe
	}
	return CodexWorkspaceAppExecutable()
}

// A file-backed home gives the child exclusive use of the freshly
// authorized grant. No running Codex or existing store is opened here.
var withCodexWorkspaceServer = func(ctx context.Context, auth json.RawMessage, use func(*codexWorkspaceServer) error) error {
	exe := codexWorkspaceExecutable()
	if exe == "" {
		return errors.New("install the Codex CLI or the official Windows Codex app to list workspaces")
	}
	home, err := os.MkdirTemp("", "magpie-codex-workspaces-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	authFile := filepath.Join(home, "auth.json")
	if err := os.WriteFile(authFile, auth, 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := proc.CommandContext(ctx, exe, "app-server", "--listen", "stdio://", "-c", `cli_auth_credentials_store="file"`)
	cmd.Dir = home
	cmd.Env = codexWorkspaceEnv(ctx, cmd.Environ(), home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return err
	}
	defer func() {
		stdin.Close()
		cancel()
		_ = cmd.Wait()
	}()
	server := &codexWorkspaceServer{authFile: authFile, in: json.NewEncoder(stdin), out: json.NewDecoder(io.LimitReader(stdout, 16<<20))}
	if err := server.call("initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "magpie", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, nil); err != nil {
		return err
	}
	if err := server.in.Encode(map[string]string{"method": "initialized"}); err != nil {
		return err
	}
	return use(server)
}

func codexWorkspaceEnv(ctx context.Context, env []string, home string) []string {
	env = netproxy.EnvWith(netproxy.Choice(ctx), env)
	out := make([]string, 0, len(env)+2)
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if !strings.EqualFold(key, "OPENAI_API_KEY") && !strings.EqualFold(key, "CODEX_HOME") &&
			!strings.EqualFold(key, "CODEX_SQLITE_HOME") {
			out = append(out, item)
		}
	}
	return append(out, "CODEX_HOME="+home, "CODEX_SQLITE_HOME="+home)
}
