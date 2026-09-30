package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	accountsURL = "https://accounts.spotify.com"
	scopes      = "user-read-playback-state user-modify-playback-state"
)

var (
	errNotLoggedIn = errors.New("not logged in to Spotify: run this plugin's binary with `login` first (see README)")
	// errForeignCallback marks a callback that does not belong to this login,
	// e.g. a stale tab from an earlier attempt or another web page.
	errForeignCallback = errors.New("state mismatch in the OAuth callback")
)

// savedToken is what login writes to disk and the plugin reads back. PKCE
// needs no client secret, so this is all there is to keep.
type savedToken struct {
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
}

// tokenPath lives outside plugins/<id>/ because installing a new version of
// the plugin replaces that folder.
func tokenPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("spotify.tokenPath: %w", err)
	}
	return filepath.Join(dir, "KeyForge", "plugin-data", pluginID, "token.json"), nil
}

func loadToken(path string) (savedToken, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the plugin's own token file
	if err != nil {
		return savedToken{}, fmt.Errorf("spotify.loadToken: %w", err)
	}
	var tok savedToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return savedToken{}, fmt.Errorf("spotify.loadToken: %w", err)
	}
	return tok, nil
}

func saveToken(path string, tok savedToken) error {
	raw, err := json.Marshal(tok) // #nosec G117 -- persisting the refresh token is the point; the file is 0600
	if err != nil {
		return fmt.Errorf("spotify.saveToken: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("spotify.saveToken: %w", err)
	}
	// Write and rename, so a crash mid-write never leaves a corrupt file: after
	// a rotation it holds the only refresh token that still works. CreateTemp
	// makes it 0600 even if an older token.json had wider permissions.
	tmp, err := os.CreateTemp(dir, "token-*.json")
	if err != nil {
		return fmt.Errorf("spotify.saveToken: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("spotify.saveToken: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("spotify.saveToken: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("spotify.saveToken: %w", err)
	}
	return nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

type oauthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *oauthError) Error() string {
	return fmt.Sprintf("spotify token request failed: %s: %s", e.Code, e.Description)
}

func requestToken(ctx context.Context, client *http.Client, tokenURL string, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("spotify.requestToken: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("spotify.requestToken: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("spotify.requestToken: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		oerr := &oauthError{}
		if json.Unmarshal(body, oerr) != nil || oerr.Code == "" {
			oerr = &oauthError{Code: resp.Status, Description: string(body)}
		}
		return tokenResponse{}, oerr
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return tokenResponse{}, fmt.Errorf("spotify.requestToken: %w", err)
	}
	return tok, nil
}

// tokens hands out access tokens, refreshing them from the saved refresh
// token. It is shared by the handlers and the volume flusher goroutine.
type tokens struct {
	path     string
	tokenURL string
	client   *http.Client
	now      func() time.Time

	mu     sync.Mutex
	saved  savedToken
	access string
	expiry time.Time
}

func (t *tokens) accessToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.access != "" && t.now().Before(t.expiry) {
		return t.access, nil
	}
	// Read the file lazily, so running `login` while the plugin is up works
	// without restarting the daemon.
	if t.saved.RefreshToken == "" {
		saved, err := loadToken(t.path)
		if errors.Is(err, fs.ErrNotExist) {
			return "", errNotLoggedIn
		}
		if err != nil {
			return "", fmt.Errorf("spotify.accessToken: %w", err)
		}
		t.saved = saved
	}

	resp, err := requestToken(ctx, t.client, t.tokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.saved.RefreshToken},
		"client_id":     {t.saved.ClientID},
	})
	var oerr *oauthError
	if errors.As(err, &oerr) && oerr.Code == "invalid_grant" {
		t.saved = savedToken{}
		return "", fmt.Errorf("spotify.accessToken: refresh token rejected, run `login` again: %w", err)
	}
	if err != nil {
		return "", fmt.Errorf("spotify.accessToken: %w", err)
	}

	t.access = resp.AccessToken
	// Refresh a minute early so a token never expires mid-request.
	t.expiry = t.now().Add(max(time.Duration(resp.ExpiresIn)*time.Second-time.Minute, 0))
	// Spotify rotates refresh tokens issued through PKCE: the old one stops
	// working, so the new one has to replace it on disk.
	if resp.RefreshToken != "" && resp.RefreshToken != t.saved.RefreshToken {
		t.saved.RefreshToken = resp.RefreshToken
		if err := saveToken(t.path, t.saved); err != nil {
			return "", fmt.Errorf("spotify.accessToken: %w", err)
		}
	}
	return t.access, nil
}

// invalidate drops the cached access token after the API rejects it.
func (t *tokens) invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.access = ""
}

type loginConfig struct {
	clientID    string
	listener    net.Listener
	accountsURL string
	client      *http.Client
	openBrowser func(url string) error
	out         io.Writer
	tokenPath   string
}

type callbackResult struct {
	code string
	err  error
}

// login runs the OAuth Authorization Code flow with PKCE: the user approves
// access in the browser, Spotify redirects to the local callback with a code,
// and the code plus the PKCE verifier are exchanged for a refresh token.
func login(ctx context.Context, cfg loginConfig) error {
	verifier, err := randomString(48)
	if err != nil {
		return fmt.Errorf("spotify.login: %w", err)
	}
	state, err := randomString(16)
	if err != nil {
		return fmt.Errorf("spotify.login: %w", err)
	}
	challenge := sha256.Sum256([]byte(verifier))
	redirectURI := "http://" + cfg.listener.Addr().String() + "/callback"

	authURL := cfg.accountsURL + "/authorize?" + url.Values{
		"client_id":             {cfg.clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {scopes},
		"state":                 {state},
		"code_challenge_method": {"S256"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
	}.Encode()

	results := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		res := callbackFrom(r.URL.Query(), state)
		if errors.Is(res.err, errForeignCallback) {
			// Ignored: only a callback carrying our state may end the login.
			http.Error(w, res.err.Error(), http.StatusBadRequest)
			return
		}
		if res.err != nil {
			http.Error(w, "Login failed: "+res.err.Error(), http.StatusBadRequest)
		} else {
			_, _ = io.WriteString(w, "Logged in to Spotify. You can close this tab.")
		}
		select {
		case results <- res:
		default: // only the first callback counts
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(cfg.listener) }()
	defer func() {
		// Shutdown rather than Close, so the browser gets the result page.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	_, _ = fmt.Fprintf(cfg.out, "Opening your browser to log in to Spotify. If it does not open, visit:\n\n%s\n\n", authURL)
	if err := cfg.openBrowser(authURL); err != nil {
		_, _ = fmt.Fprintf(cfg.out, "Could not open the browser (%v); open the URL above by hand.\n", err)
	}

	var res callbackResult
	select {
	case <-ctx.Done():
		return fmt.Errorf("spotify.login: %w", ctx.Err())
	case res = <-results:
	}
	if res.err != nil {
		return fmt.Errorf("spotify.login: %w", res.err)
	}

	tok, err := requestToken(ctx, cfg.client, cfg.accountsURL+"/api/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {redirectURI},
		"client_id":     {cfg.clientID},
		"code_verifier": {verifier},
	})
	if err != nil {
		return fmt.Errorf("spotify.login: %w", err)
	}
	if tok.RefreshToken == "" {
		return errors.New("spotify.login: Spotify returned no refresh token")
	}
	if err := saveToken(cfg.tokenPath, savedToken{ClientID: cfg.clientID, RefreshToken: tok.RefreshToken}); err != nil {
		return fmt.Errorf("spotify.login: %w", err)
	}
	_, _ = fmt.Fprintf(cfg.out, "Logged in. Token saved to %s\n", cfg.tokenPath)
	return nil
}

func callbackFrom(query url.Values, state string) callbackResult {
	if query.Get("state") != state {
		return callbackResult{err: errForeignCallback}
	}
	if e := query.Get("error"); e != "" {
		return callbackResult{err: fmt.Errorf("spotify denied access: %s", e)}
	}
	code := query.Get("code")
	if code == "" {
		return callbackResult{err: errors.New("OAuth callback without a code")}
	}
	return callbackResult{code: code}
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("spotify.randomString: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target) // #nosec G204 -- opens the Spotify authorize URL built by login
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target) // #nosec G204 -- opens the Spotify authorize URL built by login
	default:
		cmd = exec.Command("xdg-open", target) // #nosec G204 -- opens the Spotify authorize URL built by login
	}
	return cmd.Start()
}
