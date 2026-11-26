package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	codexAuthClaims           = "https://api.openai.com/auth"
	codexWorkspaceOwnerPrefix = "magpie-workspaces-"
)

func codexWorkspaceOwner(auth json.RawMessage) string {
	var a codexAuth
	if json.Unmarshal(auth, &a) != nil {
		return ""
	}
	return codexManagedWorkspaceOwner(a)
}

func codexManagedWorkspaceOwner(a codexAuth) string {
	if strings.HasPrefix(a.Tokens.LoginOwnerID, codexWorkspaceOwnerPrefix) {
		return a.Tokens.LoginOwnerID
	}
	return ""
}

func codexGrantUser(a codexAuth) string {
	return claimString(jwtClaims(a.Tokens.IDToken), codexAuthClaims, "chatgpt_user_id")
}

func codexAuthPlan(a codexAuth, id map[string]any) string {
	if codexManagedWorkspaceOwner(a) != "" &&
		codexWorkspaceToken(a.Tokens.AccessToken, codexGrantUser(a), codexAccountID(a)) {
		if plan := claimString(jwtClaims(a.Tokens.AccessToken), codexAuthClaims, "chatgpt_plan_type"); plan != "" {
			return plan
		}
	}
	return claimString(id, codexAuthClaims, "chatgpt_plan_type")
}

func codexSameAccountEmail(a, b codexAuth) bool {
	email := claimString(jwtClaims(a.Tokens.IDToken), "email")
	return email != "" && strings.EqualFold(email, claimString(jwtClaims(b.Tokens.IDToken), "email"))
}

func codexWorkspaceToken(token, user, workspace string) bool {
	c := jwtClaims(token)
	return user != "" && workspace != "" && claimString(c, codexAuthClaims, "chatgpt_user_id") == user &&
		claimString(c, codexAuthClaims, "chatgpt_account_id") == workspace
}

func codexWorkspaceFresh(token string) bool {
	exp, _ := jwtClaims(token)["exp"].(float64)
	return exp != 0 && time.Until(time.Unix(int64(exp), 0)) > 5*time.Minute
}

func codexWorkspaceAuth(auth json.RawMessage, owner, workspace, access, refresh string) (json.RawMessage, error) {
	var raw map[string]any
	if err := json.Unmarshal(auth, &raw); err != nil {
		return nil, err
	}
	tokens, _ := raw["tokens"].(map[string]any)
	if tokens == nil {
		return nil, errors.New("Codex sign-in has no tokens")
	}
	tokens["login_owner_id"] = owner
	if workspace != "" {
		tokens["account_id"] = workspace
	}
	if access != "" {
		tokens["access_token"] = access
	}
	if refresh != "" {
		tokens["refresh_token"] = refresh
	}
	return json.Marshal(raw)
}

func codexWorkspaceProbeAuth(auth json.RawMessage) (json.RawMessage, error) {
	var raw map[string]any
	if err := json.Unmarshal(auth, &raw); err != nil {
		return nil, err
	}
	tokens, _ := raw["tokens"].(map[string]any)
	if tokens == nil {
		return nil, errors.New("Codex sign-in has no tokens")
	}
	tokens["refresh_token"] = ""
	return json.Marshal(raw)
}

// Matches Codex's workspace token exchange. The authorization response is
// accepted only for the same user and the workspace the caller named.
func exchangeCodexWorkspace(ctx context.Context, token, user, workspace string) (string, error) {
	if user == "" || claimString(jwtClaims(token), codexAuthClaims, "chatgpt_user_id") != user {
		return "", errors.New("Codex returned a token for a different user")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"workspace_id": workspace})
	endpoint := strings.TrimSuffix(CodexBase, "/codex") + "/accounts/switch-workspace-token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	codexClientHeaders(req.Header, codexVersion())
	client := *http.DefaultClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Codex workspace token exchange failed (HTTP %d)", resp.StatusCode)
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil ||
		!codexWorkspaceToken(result.AccessToken, user, workspace) {
		return "", errors.New("Codex returned a token for a different user or workspace")
	}
	return result.AccessToken, nil
}

func scopeLiveCodexWorkspace(ctx context.Context, before json.RawMessage, st codexStore, a codexAuth) (string, string, error) {
	ws := codexAccountID(a)
	access, err := exchangeCodexWorkspace(ctx, a.Tokens.AccessToken, codexGrantUser(a), ws)
	if err != nil {
		return "", "", err
	}
	now, _, err := readCodexLogin(st.home, true)
	if err != nil || !sameAuth(now, before) {
		return "", "", errors.New("Codex sign-in changed during workspace renewal")
	}
	auth, err := codexWorkspaceAuth(before, a.Tokens.LoginOwnerID, ws, access, "")
	if err != nil {
		return "", "", err
	}
	if err := st.write(auth); err != nil {
		return "", "", err
	}
	return access, ws, nil
}

func syncCodexWorkspaceRows(ls []savedLogin, owner string, grant codexAuth) error {
	for i := range ls {
		if ls[i].Agent != "codex" || codexWorkspaceOwner(ls[i].Auth) != owner {
			continue
		}
		var a codexAuth
		if json.Unmarshal(ls[i].Auth, &a) != nil || a.Tokens.RefreshToken == grant.Tokens.RefreshToken {
			continue
		}
		auth, err := codexWorkspaceAuth(ls[i].Auth, owner, "", "", grant.Tokens.RefreshToken)
		if err != nil {
			return err
		}
		ls[i].Auth = auth
	}
	return nil
}

// A group keeps its latest refresh token in step across its account rows.
// Callers hold savedTokenMu so a rotation and its write are one operation
// even across Magpie processes. Rows keep their own scoped access tokens.
func keepCodexWorkspaceGrant(owner string, grant codexAuth, workspace, token string, renewed bool) error {
	loginsMu.Lock()
	defer loginsMu.Unlock()
	ls := readLogins()
	changed := false
	for i := range ls {
		l := &ls[i]
		if codexWorkspaceOwner(l.Auth) != owner {
			continue
		}
		_, ws := codexWho(l.Auth)
		var access string
		if ws == workspace {
			access = token
		}
		auth, err := codexWorkspaceAuth(l.Auth, owner, ws, access, grant.Tokens.RefreshToken)
		if err != nil {
			return err
		}
		if !bytes.Equal(auth, l.Auth) {
			l.Auth = auth
			changed = true
		}
		if renewed {
			l.Renewed = time.Now().UTC().Truncate(time.Second)
			l.Lapsed = ""
			changed = true
		}
	}
	if changed {
		return writeLogins(ls)
	}
	return nil
}

func managedCodexHome(l savedLogin, owner string) (string, codexAuth) {
	homes := []string{codexHome()}
	if l.Home != "" && l.Home != homes[0] {
		homes = append(homes, l.Home)
	}
	for _, home := range homes {
		b, _, err := readCodexLogin(home, true)
		if err == nil && codexWorkspaceOwner(b) == owner {
			var a codexAuth
			_ = json.Unmarshal(b, &a)
			return home, a
		}
	}
	return "", codexAuth{}
}

func renewCodexWorkspace(ctx context.Context, l savedLogin, owner string, force bool, stale string) (string, string, error) {
	var target codexAuth
	if json.Unmarshal(l.Auth, &target) != nil {
		return "", "", errors.New("unreadable Codex workspace sign-in")
	}
	ws, user := codexAccountID(target), codexGrantUser(target)
	access := target.Tokens.AccessToken
	if !force && access != stale && codexWorkspaceFresh(access) && codexWorkspaceToken(access, user, ws) {
		return access, ws, nil
	}

	home, grant := managedCodexHome(l, owner)
	if home != "" {
		var rejected string
		if force || grant.Tokens.AccessToken == stale {
			rejected = grant.Tokens.AccessToken
		}
		token, active, err := codexTokenRenewing(ctx, home, rejected)
		if err != nil {
			return "", "", err
		}
		if homeNow, current := managedCodexHome(l, owner); homeNow == home {
			grant = current
		} else {
			return "", "", errors.New("Codex sign-in changed during workspace renewal")
		}
		if !codexWorkspaceToken(token, user, active) {
			return "", "", errors.New("Codex returned a token for a different workspace")
		}
		if err := keepCodexWorkspaceGrant(owner, grant, active, token, rejected != ""); err != nil {
			return "", "", err
		}
		if active != ws {
			token, err = exchangeCodexWorkspace(ctx, token, user, ws)
			if err != nil {
				return "", "", err
			}
			// The exchange also selects the workspace for the next OAuth
			// refresh. Put the running Codex back on the workspace it owns.
			if _, err := exchangeCodexWorkspace(context.WithoutCancel(ctx), token, user, active); err != nil {
				return "", "", err
			}
		}
		return token, ws, keepCodexWorkspaceGrant(owner, grant, ws, token, false)
	}

	grant = target
	if !force {
		loginsMu.Lock()
		for _, peer := range readLogins() {
			if codexWorkspaceOwner(peer.Auth) != owner {
				continue
			}
			var a codexAuth
			if json.Unmarshal(peer.Auth, &a) == nil && a.Tokens.AccessToken != stale &&
				codexWorkspaceFresh(a.Tokens.AccessToken) && codexWorkspaceToken(a.Tokens.AccessToken, user, codexAccountID(a)) {
				grant = a
				break
			}
		}
		loginsMu.Unlock()
	}
	token := grant.Tokens.AccessToken
	if force || token == stale || !codexWorkspaceFresh(token) {
		var raw map[string]any
		if json.Unmarshal(l.Auth, &raw) != nil {
			return "", "", errors.New("unreadable Codex workspace sign-in")
		}
		reason := codexRefreshExpiry
		if stale != "" {
			reason = codexRefreshRecovery
		} else if force {
			reason = forcedReason(ctx)
		}
		var err error
		token, err = codexRefresh(ctx, raw, reason)
		if err != nil {
			return "", "", savedRefreshFailed("codex", l.User, err)
		}
		b, err := json.Marshal(raw)
		if err != nil {
			return "", "", err
		}
		_ = json.Unmarshal(b, &grant)
		actual := claimString(jwtClaims(token), codexAuthClaims, "chatgpt_account_id")
		if !codexWorkspaceToken(token, user, actual) {
			actual = "" // still save a refresh token already rotated upstream
		}
		if err := keepCodexWorkspaceGrant(owner, grant, actual, token, true); err != nil {
			return "", "", err
		}
	}
	if !codexWorkspaceToken(token, user, ws) {
		var err error
		token, err = exchangeCodexWorkspace(ctx, token, user, ws)
		if err != nil {
			return "", "", err
		}
	}
	return token, ws, keepCodexWorkspaceGrant(owner, grant, ws, token, false)
}
