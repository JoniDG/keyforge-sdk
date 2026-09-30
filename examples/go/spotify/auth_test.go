package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAccounts stands in for accounts.spotify.com's token endpoint.
type fakeAccounts struct {
	mu     sync.Mutex
	forms  []url.Values
	status int
	body   string
}

func newFakeAccounts(t *testing.T, status int, body string) (*fakeAccounts, *httptest.Server) {
	t.Helper()
	f := &fakeAccounts{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/token", r.URL.Path)
		assert.NoError(t, r.ParseForm())
		f.mu.Lock()
		defer f.mu.Unlock()
		f.forms = append(f.forms, r.PostForm)
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAccounts) reply(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeAccounts) requests() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.forms...)
}

func writeToken(t *testing.T, path string, tok savedToken) {
	t.Helper()
	require.NoError(t, saveToken(path, tok))
}

func TestTokensRefreshAndRotate(t *testing.T) {
	t.Parallel()

	accounts, srv := newFakeAccounts(t, http.StatusOK, `{"access_token":"a1","refresh_token":"r2","expires_in":3600}`)
	path := filepath.Join(t.TempDir(), "nested", "token.json")
	writeToken(t, path, savedToken{ClientID: "client", RefreshToken: "r1"})
	now := time.Unix(1000, 0)
	tok := &tokens{path: path, tokenURL: srv.URL + "/api/token", client: srv.Client(), now: func() time.Time { return now }}

	access, err := tok.accessToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "a1", access)
	saved, err := loadToken(path)
	require.NoError(t, err)
	assert.Equal(t, savedToken{ClientID: "client", RefreshToken: "r2"}, saved, "the rotated refresh token replaces the old one")

	_, err = tok.accessToken(t.Context())
	require.NoError(t, err)
	require.Len(t, accounts.requests(), 1, "a valid access token is reused")

	now = now.Add(time.Hour)
	accounts.reply(http.StatusOK, `{"access_token":"a2","expires_in":3600}`)
	access, err = tok.accessToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "a2", access)
	forms := accounts.requests()
	require.Len(t, forms, 2)
	assert.Equal(t, "refresh_token", forms[1].Get("grant_type"))
	assert.Equal(t, "r2", forms[1].Get("refresh_token"))
	assert.Equal(t, "client", forms[1].Get("client_id"))
}

func TestTokensNotLoggedIn(t *testing.T) {
	t.Parallel()

	tok := &tokens{path: filepath.Join(t.TempDir(), "token.json"), now: time.Now}
	_, err := tok.accessToken(t.Context())
	require.ErrorIs(t, err, errNotLoggedIn)
}

func TestTokensCorruptFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token.json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	tok := &tokens{path: path, now: time.Now}
	_, err := tok.accessToken(t.Context())
	require.ErrorContains(t, err, "spotify.loadToken")
}

func TestTokensRejectedRefreshRereadsFile(t *testing.T) {
	t.Parallel()

	accounts, srv := newFakeAccounts(t, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Refresh token revoked"}`)
	path := filepath.Join(t.TempDir(), "token.json")
	writeToken(t, path, savedToken{ClientID: "client", RefreshToken: "revoked"})
	tok := &tokens{path: path, tokenURL: srv.URL + "/api/token", client: srv.Client(), now: time.Now}

	_, err := tok.accessToken(t.Context())
	require.ErrorContains(t, err, "run `login` again")

	// A new login while the plugin runs is picked up without a restart.
	writeToken(t, path, savedToken{ClientID: "client", RefreshToken: "fresh"})
	accounts.reply(http.StatusOK, `{"access_token":"a1","expires_in":3600}`)
	access, err := tok.accessToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "a1", access)
	assert.Equal(t, "fresh", accounts.requests()[1].Get("refresh_token"))
}

func TestTokensRefreshErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "error without an OAuth body", status: http.StatusInternalServerError, body: "oops", want: "500 Internal Server Error: oops"},
		{name: "other OAuth error", status: http.StatusBadRequest, body: `{"error":"invalid_client","error_description":"Invalid client"}`, want: "invalid_client: Invalid client"},
		{name: "invalid success body", status: http.StatusOK, body: "{", want: "spotify.requestToken"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, srv := newFakeAccounts(t, tt.status, tt.body)
			path := filepath.Join(t.TempDir(), "token.json")
			writeToken(t, path, savedToken{ClientID: "client", RefreshToken: "r1"})
			tok := &tokens{path: path, tokenURL: srv.URL + "/api/token", client: srv.Client(), now: time.Now}
			_, err := tok.accessToken(t.Context())
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestTokensSaveRotatedFails(t *testing.T) {
	t.Parallel()

	_, srv := newFakeAccounts(t, http.StatusOK, `{"access_token":"a1","refresh_token":"r2","expires_in":3600}`)
	// The parent of the token path is a file, so writing the rotated token fails.
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	tok := &tokens{
		path: filepath.Join(blocker, "token.json"), tokenURL: srv.URL + "/api/token", client: srv.Client(), now: time.Now,
		saved: savedToken{ClientID: "client", RefreshToken: "r1"},
	}
	_, err := tok.accessToken(t.Context())
	require.ErrorContains(t, err, "spotify.saveToken")
}

func TestTokensUnreachable(t *testing.T) {
	t.Parallel()

	tok := &tokens{ // #nosec G101 -- fake token for a test
		tokenURL: "http://127.0.0.1:1/api/token", client: http.DefaultClient, now: time.Now,
		saved: savedToken{ClientID: "client", RefreshToken: "r1"},
	}
	_, err := tok.accessToken(t.Context())
	require.ErrorContains(t, err, "spotify.requestToken")
}

func TestTokensInvalidate(t *testing.T) {
	t.Parallel()

	tok := &tokens{access: "a1", expiry: time.Now().Add(time.Hour), now: time.Now}
	tok.invalidate()
	assert.Empty(t, tok.access)
}

// loginHarness runs login against a fake accounts server; browser plays the
// user's browser, following the authorize URL login prints.
type loginHarness struct {
	accounts  *fakeAccounts
	cfg       loginConfig
	out       *bytes.Buffer
	challenge string
}

func newLoginHarness(t *testing.T, browser func(authURL *url.URL) error) *loginHarness {
	t.Helper()
	accounts, srv := newFakeAccounts(t, http.StatusOK, `{"access_token":"a1","refresh_token":"r1","expires_in":3600}`)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	h := &loginHarness{accounts: accounts, out: &bytes.Buffer{}}
	h.cfg = loginConfig{
		clientID:    "client",
		listener:    listener,
		accountsURL: srv.URL,
		client:      srv.Client(),
		out:         h.out,
		tokenPath:   filepath.Join(t.TempDir(), "token.json"),
		openBrowser: func(raw string) error {
			authURL, err := url.Parse(raw)
			require.NoError(t, err)
			h.challenge = authURL.Query().Get("code_challenge")
			return browser(authURL)
		},
	}
	return h
}

// approve follows the redirect Spotify would send after the user approves.
func approve(query func(state string) url.Values) func(authURL *url.URL) error {
	return func(authURL *url.URL) error {
		q := authURL.Query()
		resp, err := http.Get(q.Get("redirect_uri") + "?" + query(q.Get("state")).Encode())
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
}

func approved(state string) url.Values {
	return url.Values{"code": {"the-code"}, "state": {state}}
}

func TestLogin(t *testing.T) {
	t.Parallel()

	var authQuery url.Values
	h := newLoginHarness(t, func(authURL *url.URL) error {
		authQuery = authURL.Query()
		return approve(approved)(authURL)
	})
	require.NoError(t, login(t.Context(), h.cfg))

	assert.Equal(t, "client", authQuery.Get("client_id"))
	assert.Equal(t, "code", authQuery.Get("response_type"))
	assert.Equal(t, "S256", authQuery.Get("code_challenge_method"))
	assert.Equal(t, scopes, authQuery.Get("scope"))

	forms := h.accounts.requests()
	require.Len(t, forms, 1)
	form := forms[0]
	assert.Equal(t, "authorization_code", form.Get("grant_type"))
	assert.Equal(t, "the-code", form.Get("code"))
	assert.Equal(t, authQuery.Get("redirect_uri"), form.Get("redirect_uri"))
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	assert.Equal(t, h.challenge, base64.RawURLEncoding.EncodeToString(sum[:]), "the verifier matches the challenge")

	saved, err := loadToken(h.cfg.tokenPath)
	require.NoError(t, err)
	assert.Equal(t, savedToken{ClientID: "client", RefreshToken: "r1"}, saved)
	assert.Contains(t, h.out.String(), "Logged in.")
}

func TestLoginBrowserDoesNotOpen(t *testing.T) {
	t.Parallel()

	h := newLoginHarness(t, func(authURL *url.URL) error {
		// The user opens the printed URL by hand.
		require.NoError(t, approve(approved)(authURL))
		return errors.New("no browser")
	})
	require.NoError(t, login(t.Context(), h.cfg))
	assert.Contains(t, h.out.String(), "Could not open the browser (no browser)")
}

func TestLoginCallbackErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query func(state string) url.Values
		want  string
	}{
		{name: "user denied", query: func(state string) url.Values { return url.Values{"error": {"access_denied"}, "state": {state}} }, want: "denied access: access_denied"},
		{name: "no code", query: func(state string) url.Values { return url.Values{"state": {state}} }, want: "without a code"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newLoginHarness(t, approve(tt.query))
			require.ErrorContains(t, login(t.Context(), h.cfg), tt.want)
			assert.Empty(t, h.accounts.requests())
		})
	}
}

func TestLoginIgnoresForeignCallback(t *testing.T) {
	t.Parallel()

	h := newLoginHarness(t, func(authURL *url.URL) error {
		// A stale tab or another web page hits the callback first.
		resp, err := http.Get(authURL.Query().Get("redirect_uri") + "?code=forged&state=forged")
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		return approve(approved)(authURL)
	})
	require.NoError(t, login(t.Context(), h.cfg))
	forms := h.accounts.requests()
	require.Len(t, forms, 1)
	assert.Equal(t, "the-code", forms[0].Get("code"))
}

func TestLoginWithoutRefreshToken(t *testing.T) {
	t.Parallel()

	h := newLoginHarness(t, approve(approved))
	h.accounts.reply(http.StatusOK, `{"access_token":"a1","expires_in":3600}`)
	require.ErrorContains(t, login(t.Context(), h.cfg), "no refresh token")
}

func TestSaveTokenTightensPermissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644)) // #nosec G306 -- a token file left too open by someone else
	writeToken(t, path, savedToken{ClientID: "client", RefreshToken: "r1"})
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temp file is left behind")
}

func TestLoginTokenExchangeFails(t *testing.T) {
	t.Parallel()

	h := newLoginHarness(t, approve(approved))
	h.accounts.reply(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Invalid authorization code"}`)
	require.ErrorContains(t, login(t.Context(), h.cfg), "Invalid authorization code")
}

func TestLoginSaveFails(t *testing.T) {
	t.Parallel()

	h := newLoginHarness(t, approve(approved))
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	h.cfg.tokenPath = filepath.Join(blocker, "token.json")
	require.ErrorContains(t, login(t.Context(), h.cfg), "spotify.saveToken")
}

func TestLoginCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	h := newLoginHarness(t, func(*url.URL) error {
		cancel() // the user gives up before approving
		return nil
	})
	require.ErrorIs(t, login(ctx, h.cfg), context.Canceled)
}
