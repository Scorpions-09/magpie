package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func workspaceTestToken(workspace string, exp time.Time) string {
	return fakeJWT(map[string]any{"email": "member@example.com", "exp": exp.Unix(),
		codexAuthClaims: map[string]string{"chatgpt_account_id": workspace, "chatgpt_user_id": "member", "chatgpt_plan_type": "enterprise"}})
}

func workspaceTestLogin(t *testing.T, workspace, owner, refresh string, exp time.Time) savedLogin {
	t.Helper()
	l, err := codexLogin(workspaceTestToken("one", exp), workspaceTestToken(workspace, exp), refresh, "")
	if err != nil {
		t.Fatal(err)
	}
	l.Auth, err = codexWorkspaceAuth(l.Auth, owner, workspace, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

type workspaceTestWriter func([]byte) (int, error)

func (f workspaceTestWriter) Write(b []byte) (int, error) { return f(b) }

func TestCodexWorkspaceExecutableFallsBackToDesktop(t *testing.T) {
	oldCLI, oldApp := codexExecutable, CodexWorkspaceAppExecutable
	t.Cleanup(func() { codexExecutable, CodexWorkspaceAppExecutable = oldCLI, oldApp })
	CodexWorkspaceAppExecutable = func() string { return "store-codex" }
	codexExecutable = func() string { return "" }
	if got := codexWorkspaceExecutable(); got != "store-codex" {
		t.Fatalf("workspace discovery picked %q with no CLI on the desktop PATH", got)
	}
	codexExecutable = func() string { return "standalone-codex" }
	if got := codexWorkspaceExecutable(); got != "standalone-codex" {
		t.Fatalf("workspace discovery picked %q with a standalone CLI", got)
	}
}

func TestCodexWorkspaceBrowserSignInImportsAndRetries(t *testing.T) {
	signIn(t)
	ids := []string{"one", "two", "three", "four", "five"}
	want := make([]CodexWorkspace, len(ids))
	for i, id := range ids {
		want[i] = CodexWorkspace{ID: id, Name: "Workspace " + id}
	}
	now := time.Now().Add(time.Hour)
	start := workspaceTestLogin(t, "one", "", "a-refresh-token-kept-on-the-server", now)
	var a codexAuth
	_ = json.Unmarshal(start.Auth, &a)
	var oauthCalls atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/accounts/switch-workspace-token" {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": workspaceTestToken("one", now)})
			return
		}
		oauthCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": a.Tokens.IDToken,
			"access_token": a.Tokens.AccessToken, "refresh_token": a.Tokens.RefreshToken})
	}))
	t.Cleanup(oauth.Close)
	t.Cleanup(CodexTokenURLForTest(oauth.URL))
	oldBase := CodexBase
	CodexBase = oauth.URL + "/codex"
	t.Cleanup(func() { CodexBase = oldBase })

	old := withCodexWorkspaceServer
	t.Cleanup(func() { withCodexWorkspaceServer = old })
	failures := map[string]*atomic.Bool{"three": {}, "five": {}}
	overlap := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	withCodexWorkspaceServer = func(ctx context.Context, auth json.RawMessage, use func(*codexWorkspaceServer) error) error {
		file := filepath.Join(t.TempDir(), "auth.json")
		if err := os.WriteFile(file, auth, 0o600); err != nil {
			return err
		}
		owner := codexWorkspaceOwner(auth)
		if owner != "" {
			var probe codexAuth
			if json.Unmarshal(auth, &probe) != nil || probe.Tokens.RefreshToken != "" {
				t.Error("a verification worker received the rotating refresh grant")
			}
		}
		id := owner
		if id == "" {
			id = "member"
		}
		current := "one"
		var replies bytes.Buffer
		writer := workspaceTestWriter(func(b []byte) (int, error) {
			var req struct {
				ID     int
				Method string
				Params struct {
					AccountID                string
					AllowBrowserLogin        bool
					RefreshWorkspaceMetadata bool
				}
			}
			if err := json.Unmarshal(b, &req); err != nil {
				return 0, err
			}
			reply := map[string]any{"id": req.ID}
			switch req.Method {
			case "accountSession/list":
				items := want
				if !req.Params.RefreshWorkspaceMetadata {
					items = []CodexWorkspace{{ID: current}}
				}
				reply["result"] = codexWorkspaceSessions{Active: id,
					Items: []codexWorkspaceSession{{ID: id, UserID: "member", Current: current, Workspaces: items}}}
			case "accountSession/switch":
				if req.Params.AllowBrowserLogin {
					t.Error("the batch unexpectedly opened another browser login")
				}
				if req.Params.AccountID == "two" || req.Params.AccountID == "four" {
					overlap <- req.Params.AccountID
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				if failure := failures[req.Params.AccountID]; failure != nil && failure.CompareAndSwap(false, true) {
					reply["error"] = map[string]any{"code": -32602, "message": "workspace temporarily unavailable"}
					break
				}
				current = req.Params.AccountID
				wire, err := codexWorkspaceAuth(auth, owner, current, workspaceTestToken(current, now), "")
				if err != nil {
					return 0, err
				}
				if err := os.WriteFile(file, wire, 0o600); err != nil {
					return 0, err
				}
				reply["result"] = map[string]any{}
			default:
				return 0, fmt.Errorf("unexpected method %s", req.Method)
			}
			_, _ = replies.WriteString("{\"method\":\"account/updated\"}\n")
			return len(b), json.NewEncoder(&replies).Encode(reply)
		})
		return use(&codexWorkspaceServer{authFile: file, in: json.NewEncoder(writer), out: json.NewDecoder(&replies)})
	}
	s := &signInFlow{st: SignInState{ID: randomToken(9), Agent: "codex", State: "waiting"}, state: "ours",
		done: make(chan struct{}), discoverWorkspaces: true}
	signIns.Lock()
	signIns.m[s.st.ID] = s
	signIns.Unlock()
	t.Cleanup(func() {
		CancelSignIn(s.st.ID)
		signIns.Lock()
		delete(signIns.m, s.st.ID)
		signIns.Unlock()
	})
	recorder := httptest.NewRecorder()
	s.callback(recorder, httptest.NewRequest(http.MethodGet, "/auth/callback?state=ours&code=ours", nil))
	st := s.status()
	if st.State != "workspaces" || len(st.Workspaces) != 5 || st.Workspaces[3].Name != want[3].Name {
		t.Fatalf("workspace chooser: %+v", st)
	}
	if b, _ := json.Marshal(st); strings.Contains(string(b), a.Tokens.RefreshToken) {
		t.Fatal("the workspace chooser exposed the grant")
	}
	if err := ImportCodexWorkspaces(st.ID, []string{"arbitrary"}); err == nil {
		t.Fatal("accepted a workspace outside this login")
	}
	if err := ImportCodexWorkspaces(st.ID, ids); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-overlap:
		case <-time.After(10 * time.Second):
			t.Fatal("separate workspace validations did not overlap")
		}
	}
	progress, ok := CodexWorkspaceImport()
	if !ok || progress.State != "importing_workspaces" || progress.Workspaces[0].Status != "verified" {
		t.Fatalf("no live progress in a second window: %+v", progress)
	}
	unblock()
	st = waitWorkspaceState(t, s, "workspaces")
	if st.Workspaces[2].Status != "failed" || st.Workspaces[4].Status != "failed" || st.Workspaces[3].Status != "added" {
		t.Fatalf("partial results: %+v", st.Workspaces)
	}
	if s.finish(SignInState{State: "browser_timeout"}) {
		t.Fatal("the browser login timeout discarded the retry picker")
	}
	reattached, err := StartSignInWith("codex", SignInOptions{DiscoverWorkspaces: true})
	if err != nil || reattached.ID != st.ID || reattached.Workspaces[4].Status != "failed" {
		t.Fatalf("opening the picker again lost failures: %+v, %v", reattached, err)
	}
	if err := ImportCodexWorkspaces(st.ID, []string{"three"}); err != nil {
		t.Fatal(err)
	}
	st = waitWorkspaceState(t, s, "workspaces")
	if st.Workspaces[0].Status != "added" || st.Workspaces[2].Status != "added" || st.Workspaces[4].Status != "failed" {
		t.Fatalf("retry touched another workspace: %+v", st.Workspaces)
	}
	if err := ImportCodexWorkspaces(st.ID, []string{"five"}); err != nil {
		t.Fatal(err)
	}
	st = waitWorkspaceState(t, s, "done")
	if st.Workspaces[0].Status != "added" || st.Workspaces[4].Status != "added" || oauthCalls.Load() != 1 {
		t.Fatalf("retry: %+v, login calls %d", st.Workspaces, oauthCalls.Load())
	}
	found := map[string]bool{}
	var owner string
	for _, l := range readLogins() {
		_, ws := codexWho(l.Auth)
		var auth codexAuth
		_ = json.Unmarshal(l.Auth, &auth)
		if found[ws] || !codexWorkspaceToken(auth.Tokens.AccessToken, "member", ws) {
			t.Fatalf("duplicate or wrongly scoped workspace %q", ws)
		}
		if owner == "" {
			owner = codexWorkspaceOwner(l.Auth)
		}
		if owner == "" || owner != codexWorkspaceOwner(l.Auth) {
			t.Fatal("the workspaces did not share the authorized login session")
		}
		found[ws] = true
	}
	if len(found) != 5 {
		t.Fatalf("saved workspaces: %v", found)
	}
}

func waitWorkspaceState(t *testing.T, s *signInFlow, state string) SignInState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.status(); st.State == state {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waiting for %q, got %+v", state, s.status())
	return SignInState{}
}

func TestCodexWorkspaceRenewalsShareRotation(t *testing.T) {
	signIn(t)
	oldBase := CodexBase
	t.Cleanup(func() { CodexBase = oldBase })
	owner := codexWorkspaceOwnerPrefix + "shared"
	expired, fresh := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	one := workspaceTestLogin(t, "one", owner, "first-grant", expired)
	two := workspaceTestLogin(t, "two", owner, "first-grant", expired)
	two.User += " · two"
	if err := writeLogins([]savedLogin{one, two}); err != nil {
		t.Fatal(err)
	}
	var refreshes, exchanges atomic.Int32
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refreshes.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["refresh_token"] != "first-grant" {
				t.Error("the shared refresh token was used again")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": workspaceTestToken("one", fresh),
				"refresh_token": "rotated-grant"})
		case "/accounts/switch-workspace-token":
			exchanges.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": workspaceTestToken(body["workspace_id"], fresh)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(vendor.Close)
	t.Cleanup(CodexTokenURLForTest(vendor.URL + "/token"))
	CodexBase = vendor.URL + "/codex"
	var wg sync.WaitGroup
	for _, l := range []savedLogin{one, two} {
		wg.Go(func() {
			token, workspace, err := savedLoginToken(t.Context(), "codex", l.User)
			_, want := codexWho(l.Auth)
			if err != nil || workspace != want || !codexWorkspaceToken(token, "member", want) {
				t.Errorf("%s renewal used workspace %q: %v", want, workspace, err)
			}
		})
	}
	wg.Wait()
	if refreshes.Load() != 1 || exchanges.Load() != 1 {
		t.Fatalf("refreshes %d, workspace exchanges %d", refreshes.Load(), exchanges.Load())
	}
	for _, l := range readLogins() {
		var a codexAuth
		_ = json.Unmarshal(l.Auth, &a)
		if a.Tokens.RefreshToken != "rotated-grant" || !codexWorkspaceToken(a.Tokens.AccessToken, "member", codexAccountID(a)) {
			t.Fatalf("workspace did not keep the rotated grant and its own access token: %s", l.User)
		}
	}
}

func TestCodexWorkspaceRenewalPreservesTheLiveWorkspace(t *testing.T) {
	home := signIn(t)
	owner := codexWorkspaceOwnerPrefix + "live"
	expired, fresh := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	one := workspaceTestLogin(t, "one", owner, "live-grant", expired)
	two := workspaceTestLogin(t, "two", owner, "live-grant", expired)
	two.User += " · two"
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), one.Auth, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeLogins([]savedLogin{one, two}); err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	var selected atomic.Value
	selected.Store("two")
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refreshes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token": workspaceTestToken(selected.Load().(string), fresh), "refresh_token": "rotated-live-grant",
			})
		case "/accounts/switch-workspace-token":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			ws := body["workspace_id"]
			selected.Store(ws)
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": workspaceTestToken(ws, fresh)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(vendor.Close)
	t.Cleanup(CodexTokenURLForTest(vendor.URL + "/token"))
	oldBase := CodexBase
	t.Cleanup(func() { CodexBase = oldBase })
	CodexBase = vendor.URL + "/codex"

	token, workspace, err := savedLoginToken(t.Context(), "codex", two.User)
	if err != nil || workspace != "two" || !codexWorkspaceToken(token, "member", "two") {
		t.Fatalf("saved workspace: %q, %v", workspace, err)
	}
	var live codexAuth
	wire, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil || json.Unmarshal(wire, &live) != nil || !codexWorkspaceToken(live.Tokens.AccessToken, "member", "one") ||
		live.Tokens.RefreshToken != "rotated-live-grant" || selected.Load().(string) != "one" || refreshes.Load() != 1 {
		t.Fatalf("the saved account's renewal moved the live Codex session: %v", err)
	}
}
