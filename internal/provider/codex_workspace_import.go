package provider

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/redact"
)

func (s *signInFlow) offerCodexWorkspaces(ctx context.Context, l savedLogin) {
	ctx, cancel := s.workspaceContext(ctx)
	defer cancel()
	_, current := codexWho(l.Auth)
	var workspaces []CodexWorkspace
	err := withCodexWorkspaceServer(ctx, l.Auth, func(server *codexWorkspaceServer) error {
		session, listErr := server.list(true)
		auth, err := server.auth()
		if err != nil {
			return err
		}
		var a codexAuth
		var original codexAuth
		_ = json.Unmarshal(l.Auth, &original)
		if json.Unmarshal(auth, &a) != nil || codexGrantUser(a) == "" || codexGrantUser(a) != codexGrantUser(original) ||
			codexAccountID(a) != current {
			return errors.New("Codex reported a different ChatGPT sign-in")
		}
		l.Auth = auth
		if listErr != nil {
			return listErr
		}
		if session.UserID != codexGrantUser(a) || session.Current != current {
			return errors.New("Codex reported a different ChatGPT sign-in")
		}
		for _, ws := range session.Workspaces {
			if ws.ID == "" || slices.ContainsFunc(workspaces, func(w CodexWorkspace) bool { return w.ID == ws.ID }) {
				continue
			}
			ws.Current = ws.ID == current
			if ws.Name == "" {
				ws.Name = ws.ID
			}
			workspaces = append(workspaces, ws)
		}
		return nil
	})
	if !slices.ContainsFunc(workspaces, func(w CodexWorkspace) bool { return w.Current }) && current != "" {
		workspaces = append(workspaces, CodexWorkspace{ID: current, Name: l.User, Current: true})
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.workspaceLogin = l
	s.st.State, s.st.Workspaces = "workspaces", workspaces
	s.st.URL, s.st.Code = "", ""
	if err != nil {
		s.st.Error = redact.Scrub(err.Error())
	}
	srv := s.srv
	s.mu.Unlock()
	if srv != nil {
		go stopWorkspaceCallback(srv)
	}
}

func stopWorkspaceCallback(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func workspaceImportState(state string) bool {
	return state == "workspaces" || state == "importing_workspaces"
}

// CodexWorkspaceImport returns the picker or the batch running in this
// process, including from a newly opened window.
func CodexWorkspaceImport() (SignInState, bool) {
	signIns.Lock()
	defer signIns.Unlock()
	for _, s := range signIns.m {
		if st := s.status(); st.Agent == "codex" && workspaceImportState(st.State) {
			return st, true
		}
	}
	return SignInState{}, false
}

// ImportCodexWorkspaces claims a selection from this sign-in's workspace
// list. The GUI reads the outcome from the same sign-in it already polls.
func ImportCodexWorkspaces(id string, ids []string) error {
	signIns.Lock()
	s := signIns.m[id]
	signIns.Unlock()
	if s == nil {
		return errSignInOver
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || s.st.State != "workspaces" || s.workspaceLogin.Agent != "codex" {
		return errors.New("this sign-in has no workspaces ready to import")
	}
	if len(ids) == 0 || len(ids) > len(s.st.Workspaces) {
		return errors.New("choose at least one listed workspace")
	}
	chosen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if chosen[id] || !slices.ContainsFunc(s.st.Workspaces, func(w CodexWorkspace) bool { return w.ID == id }) {
			return errors.New("choose distinct workspaces from this sign-in")
		}
		chosen[id] = true
	}
	workspaces := slices.Clone(s.st.Workspaces)
	for i := range workspaces {
		w := &workspaces[i]
		if chosen[w.ID] && w.Status != "added" && w.Status != "exists" {
			w.Status, w.Error = "pending", ""
		}
	}
	s.st.Workspaces = workspaces
	s.st.Error = ""
	s.st.State = "importing_workspaces"
	go s.importCodexWorkspaces(s.workspaceLogin, slices.Clone(workspaces), chosen)
	return nil
}

func (s *signInFlow) importCodexWorkspaces(l savedLogin, workspaces []CodexWorkspace, chosen map[string]bool) {
	started := time.Now()
	ctx, deadline := context.WithTimeout(s.via(context.Background()), 4*time.Minute)
	defer deadline()
	ctx, cancel := s.workspaceContext(ctx)
	defer cancel()
	_, current := codexWho(l.Auth)
	owner := codexWorkspaceOwner(l.Auth)
	if len(workspaces) == 1 && chosen[current] && owner == "" {
		var using bool
		err := s.keep(func() (err error) { using, err = addLoginNamed(&l); return err })
		if err == nil {
			s.finish(SignInState{State: "done", User: l.User, Plan: l.Plan, Using: using})
			return
		}
		workspaces[0].Status, workspaces[0].Error = "failed", redact.Scrub(err.Error())
		s.workspaceImportResult(l, workspaces, err)
		return
	}

	var selected []savedLogin
	pending := false
	for i := range workspaces {
		w := &workspaces[i]
		if !chosen[w.ID] {
			continue
		}
		if w.Status == "added" || w.Status == "exists" || codexWorkspaceExists(l, w.ID) {
			w.Status = "exists"
			s.reportCodexWorkspace(*w)
			continue
		}
		w.Status = "pending"
		pending = true
	}
	// On a retry an earlier import may have made this grant renewable by
	// the gateway. Wait for that renewal before staging a further switch.
	if owner != "" {
		savedTokenMu.Lock()
		l = latestCodexWorkspaceGrant(l, owner)
	}
	if pending {
		var err error
		l, selected, err = collectCodexWorkspaces(ctx, l, workspaces, s.reportCodexWorkspace)
		if err != nil {
			for i := range workspaces {
				if workspaces[i].Status == "pending" {
					workspaces[i].Status, workspaces[i].Error = "failed", redact.Scrub(err.Error())
				}
			}
		}
	}
	if owner == "" {
		savedTokenMu.Lock()
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		savedTokenMu.Unlock()
		return
	}
	results, err := storeCodexWorkspaces(l, selected)
	var added string
	for i := range workspaces {
		w := &workspaces[i]
		if result, ok := results[w.ID]; ok && err == nil {
			w.Status = result.status
			if result.status == "added" {
				if added == "" {
					added = result.user
				}
				if s.chosen {
					_ = keepSignInProxy("codex", result.user, s.proxy)
				}
			}
		}
		if (w.Status == "pending" || w.Status == "verified") && err != nil {
			w.Status, w.Error = "failed", redact.Scrub(err.Error())
		}
	}
	s.st.State, s.st.Workspaces, s.st.Error = "workspaces", workspaces, ""
	s.workspaceLogin = l
	failed := slices.ContainsFunc(workspaces, func(w CodexWorkspace) bool { return w.Status == "failed" })
	if !failed {
		s.kept = true
	}
	s.mu.Unlock()
	savedTokenMu.Unlock()
	log.Printf("codex: checked %d selected workspaces in %s (%d prepared, retry needed: %t)", len(chosen), time.Since(started).Round(time.Second), len(selected), failed)
	if !failed {
		s.finish(SignInState{State: "done", User: added, Workspaces: workspaces})
	}
}

func (s *signInFlow) reportCodexWorkspace(w CodexWorkspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || s.st.State != "importing_workspaces" {
		return
	}
	rows := slices.Clone(s.st.Workspaces)
	for i := range rows {
		if rows[i].ID == w.ID {
			rows[i] = w
			s.st.Workspaces = rows
			return
		}
	}
}

func (s *signInFlow) workspaceContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (s *signInFlow) workspaceImportResult(l savedLogin, workspaces []CodexWorkspace, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.finished {
		s.workspaceLogin = l
		s.st.State, s.st.Workspaces, s.st.Error = "workspaces", workspaces, redact.Scrub(err.Error())
	}
}

const codexWorkspaceWorkers = 3

func collectCodexWorkspaces(ctx context.Context, l savedLogin, workspaces []CodexWorkspace, report func(CodexWorkspace)) (savedLogin, []savedLogin, error) {
	owner := codexWorkspaceOwner(l.Auth)
	if owner == "" {
		owner = codexWorkspaceOwnerPrefix + randomToken(24)
	}
	var err error
	l.Auth, err = codexWorkspaceAuth(l.Auth, owner, "", "", "")
	if err != nil {
		return l, nil, err
	}
	var a codexAuth
	if err := json.Unmarshal(l.Auth, &a); err != nil {
		return l, nil, err
	}
	current, user := codexAccountID(a), codexGrantUser(a)
	// The workers can switch the session and validate routing using its
	// access token. Only Magpie retains the rotating refresh grant.
	probe, err := codexWorkspaceProbeAuth(l.Auth)
	if err != nil {
		return l, nil, err
	}
	results := make([]savedLogin, len(workspaces))
	todo := make([]int, 0, len(workspaces))
	result := func(i int, b json.RawMessage, err error) {
		w := &workspaces[i]
		if err == nil {
			results[i], err = verifiedCodexWorkspace(b, owner, user, w.ID)
		}
		if err != nil {
			w.Status, w.Error = "failed", redact.Scrub(err.Error())
		} else {
			w.Status, w.Error = "verified", ""
		}
		report(*w)
	}
	for i, w := range workspaces {
		if w.Status != "pending" {
			continue
		}
		if w.ID == current && codexWorkspaceToken(a.Tokens.AccessToken, user, current) {
			result(i, l.Auth, nil)
		} else {
			todo = append(todo, i)
		}
	}
	var wg sync.WaitGroup
	var switched atomic.Bool
	workers := min(codexWorkspaceWorkers, len(todo))
	for first := range workers {
		wg.Go(func() {
			next := first
			err := withCodexWorkspaceServer(ctx, probe, func(server *codexWorkspaceServer) error {
				session, err := server.list(false)
				if err != nil {
					return err
				}
				if session.ID != owner || session.Current != current || session.UserID != user {
					return errors.New("Codex reported a different workspace session")
				}
				for ; next < len(todo); next += workers {
					if err := ctx.Err(); err != nil {
						return err
					}
					i := todo[next]
					switched.Store(true)
					err := server.selectWorkspace(session.ID, workspaces[i].ID)
					var b json.RawMessage
					if err == nil {
						b, err = server.auth()
					}
					result(i, b, err)
				}
				return nil
			})
			for ; next < len(todo); next += workers {
				result(todo[next], nil, err)
			}
		})
	}
	wg.Wait()
	var selected []savedLogin
	for _, l := range results {
		if l.Agent != "" {
			selected = append(selected, l)
		}
	}
	if switched.Load() {
		// A switch picks the server-side workspace for the next refresh.
		// Restore it once, after the last worker (including on cancellation).
		access, err := exchangeCodexWorkspace(context.WithoutCancel(ctx), a.Tokens.AccessToken, user, current)
		if err == nil {
			l.Auth, err = codexWorkspaceAuth(l.Auth, owner, current, access, "")
			if err != nil {
				return l, selected, err
			}
		} else {
			log.Print("codex: could not restore the starting workspace after import")
		}
	}
	return l, selected, nil
}

func verifiedCodexWorkspace(b json.RawMessage, owner, user, workspace string) (savedLogin, error) {
	var a codexAuth
	if json.Unmarshal(b, &a) != nil || a.Tokens.LoginOwnerID != owner || codexGrantUser(a) != user ||
		!codexWorkspaceToken(a.Tokens.AccessToken, user, workspace) {
		return savedLogin{}, errors.New("Codex returned credentials for a different workspace")
	}
	plan := claimString(jwtClaims(a.Tokens.AccessToken), codexAuthClaims, "chatgpt_plan_type")
	return savedLogin{Agent: "codex", User: codexUserWithPlan(jwtClaims(a.Tokens.IDToken), plan), Plan: plan, Auth: b, On: true}, nil
}

func codexWorkspaceExists(l savedLogin, workspace string) bool {
	var a codexAuth
	_ = json.Unmarshal(l.Auth, &a)
	loginsMu.Lock()
	defer loginsMu.Unlock()
	for _, row := range readLogins() {
		if workspaceMatches(row, a, workspace) {
			return true
		}
	}
	live, ok := liveLogin("codex")
	return ok && workspaceMatches(live, a, workspace)
}

type codexWorkspaceResult struct{ user, status string }

// storeCodexWorkspaces runs with savedTokenMu held, like a switch of the
// active login. Account settings and duplicate workspaces are kept.
func storeCodexWorkspaces(source savedLogin, selected []savedLogin) (map[string]codexWorkspaceResult, error) {
	results := make(map[string]codexWorkspaceResult, len(selected))
	owner := codexWorkspaceOwner(source.Auth)
	if len(selected) == 0 && owner == "" {
		return results, nil
	}
	codexMu.Lock()
	defer codexMu.Unlock()
	loginsMu.Lock()
	defer loginsMu.Unlock()
	ls := readLogins()
	var grant codexAuth
	if owner == "" || json.Unmarshal(source.Auth, &grant) != nil || grant.Tokens.RefreshToken == "" {
		return nil, errors.New("Codex did not return a renewable sign-in")
	}
	if err := syncCodexWorkspaceRows(ls, owner, grant); err != nil {
		return nil, err
	}
	live, haveLive := liveLogin("codex")
	var first *savedLogin
	for _, row := range selected {
		_, ws := codexWho(row.Auth)
		if slices.ContainsFunc(ls, func(old savedLogin) bool { return workspaceMatches(old, grant, ws) }) ||
			haveLive && workspaceMatches(live, grant, ws) {
			results[ws] = codexWorkspaceResult{status: "exists"}
			continue
		}
		var err error
		row.Auth, err = codexWorkspaceAuth(row.Auth, grant.Tokens.LoginOwnerID, ws, "", grant.Tokens.RefreshToken)
		if err != nil {
			return nil, err
		}
		row.Seen = time.Now().UTC().Truncate(time.Second)
		row.Renewed = row.Seen
		row.User = codexName(ls, row)
		ls = append(ls, row)
		if first == nil || ws == codexAccountID(grant) {
			first = &row
		}
		results[ws] = codexWorkspaceResult{user: row.User, status: "added"}
	}
	// As with adding a single account, a first sign-in gives Codex an
	// active account so the other saved workspaces can be used beside it.
	if !haveLive && first != nil {
		if err := saveCodexLogin(codexHome(), first.Auth); err != nil {
			return nil, err
		}
		forgetCodexSignInGone(codexHome())
	}
	if err := writeLogins(ls); err != nil {
		return nil, err
	}
	loginsSeenAt.Store(0)
	forgetAccountCaches()
	return results, nil
}

// The retry must stage the most recent refresh token even if the durable
// rows have already rotated it since the picker was drawn.
func latestCodexWorkspaceGrant(l savedLogin, owner string) savedLogin {
	home, grant := managedCodexHome(l, owner)
	if home != "" {
		if auth, _, err := readCodexLogin(home, true); err == nil && codexWorkspaceOwner(auth) == owner {
			l.Auth = auth
			return l
		}
	}
	if grant.Tokens.RefreshToken == "" {
		loginsMu.Lock()
		for _, row := range readLogins() {
			if codexWorkspaceOwner(row.Auth) == owner && json.Unmarshal(row.Auth, &grant) == nil {
				break
			}
		}
		loginsMu.Unlock()
	}
	if grant.Tokens.RefreshToken != "" {
		if auth, err := codexWorkspaceAuth(l.Auth, owner, "", "", grant.Tokens.RefreshToken); err == nil {
			l.Auth = auth
		}
	}
	return l
}

func workspaceMatches(l savedLogin, grant codexAuth, workspace string) bool {
	var a codexAuth
	if l.Agent != "codex" || json.Unmarshal(l.Auth, &a) != nil || codexAccountID(a) != workspace {
		return false
	}
	if user := codexGrantUser(a); user != "" && codexGrantUser(grant) != "" {
		return user == codexGrantUser(grant)
	}
	return codexSameAccountEmail(a, grant)
}
