package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI stands in for api.spotify.com. Routes are keyed by "METHOD path";
// any other request answers 204, like Spotify's player commands do.
type fakeAPI struct {
	t      *testing.T
	routes map[string]http.HandlerFunc

	mu    sync.Mutex
	calls []string
}

func newFakeAPI(t *testing.T, routes map[string]http.HandlerFunc) (*player, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{t: t, routes: routes}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	p := &player{
		baseURL: srv.URL,
		client:  srv.Client(),
		tokens:  &tokens{access: "access-1", expiry: time.Now().Add(time.Hour), now: time.Now},
	}
	return p, api
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	assert.Equal(f.t, "Bearer access-1", r.Header.Get("Authorization"))
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	f.mu.Unlock()
	if h, ok := f.routes[r.Method+" "+r.URL.Path]; ok {
		h(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeAPI) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAPI) count(prefix string) int {
	n := 0
	for _, c := range f.called() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestPlayerCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(p *player) error
		want string
	}{
		{name: "play", run: func(p *player) error { return p.play(t.Context()) }, want: "PUT /me/player/play"},
		{name: "pause", run: func(p *player) error { return p.pause(t.Context()) }, want: "PUT /me/player/pause"},
		{name: "next", run: func(p *player) error { return p.next(t.Context()) }, want: "POST /me/player/next"},
		{name: "previous", run: func(p *player) error { return p.previous(t.Context()) }, want: "POST /me/player/previous"},
		{name: "set volume", run: func(p *player) error { return p.setVolume(t.Context(), 40) }, want: "PUT /me/player/volume?volume_percent=40"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, api := newFakeAPI(t, nil)
			require.NoError(t, tt.run(p))
			assert.Equal(t, []string{tt.want}, api.called())
		})
	}
}

func TestPlayerState(t *testing.T) {
	t.Parallel()

	p, _ := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /me/player": respond(http.StatusOK, `{"is_playing":true,"device":{"volume_percent":42}}`),
	})
	st, err := p.state(t.Context())
	require.NoError(t, err)
	assert.True(t, st.IsPlaying)
	level, err := p.volume(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 42, level)
}

func TestPlayerErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			name:    "nothing playing anywhere",
			handler: respond(http.StatusNoContent, ""),
			want:    "no active Spotify device",
		},
		{
			name:    "no active device",
			handler: respond(http.StatusNotFound, `{"error":{"status":404,"message":"Player command failed: No active device found","reason":"NO_ACTIVE_DEVICE"}}`),
			want:    "no active Spotify device",
		},
		{
			name:    "free account",
			handler: respond(http.StatusForbidden, `{"error":{"status":403,"message":"Player command failed: Premium required","reason":"PREMIUM_REQUIRED"}}`),
			want:    "requires Spotify Premium",
		},
		{
			name: "rate limited",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			want: "retry after 3s",
		},
		{
			name:    "other error with a message",
			handler: respond(http.StatusBadGateway, `{"error":{"status":502,"message":"Bad gateway"}}`),
			want:    "spotify API: 502 Bad gateway",
		},
		{
			name:    "other error without a body",
			handler: respond(http.StatusInternalServerError, "oops"),
			want:    "spotify API: 500 500 Internal Server Error",
		},
		{
			name:    "device without volume control",
			handler: respond(http.StatusOK, `{"is_playing":false,"device":{"volume_percent":null}}`),
			want:    "does not support volume control",
		},
		{
			name:    "invalid state body",
			handler: respond(http.StatusOK, `{`),
			want:    "spotify.state",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newFakeAPI(t, map[string]http.HandlerFunc{"GET /me/player": tt.handler})
			_, err := p.volume(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestPlayerCommandError(t *testing.T) {
	t.Parallel()

	p, _ := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /me/player/next": respond(http.StatusForbidden, `{"error":{"status":403,"message":"Premium required","reason":"PREMIUM_REQUIRED"}}`),
	})
	err := p.next(t.Context())
	var apiErr *apiError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "PREMIUM_REQUIRED", apiErr.Reason)
}

func TestPlayerUnauthorizedDropsAccessToken(t *testing.T) {
	t.Parallel()

	p, _ := newFakeAPI(t, map[string]http.HandlerFunc{
		"PUT /me/player/play": respond(http.StatusUnauthorized, `{"error":{"status":401,"message":"The access token expired"}}`),
	})
	require.Error(t, p.play(t.Context()))
	assert.Empty(t, p.tokens.access)
}

func TestPlayerNotLoggedIn(t *testing.T) {
	t.Parallel()

	p, api := newFakeAPI(t, nil)
	p.tokens = &tokens{path: filepath.Join(t.TempDir(), "token.json"), now: time.Now}
	require.ErrorIs(t, p.play(t.Context()), errNotLoggedIn)
	assert.Empty(t, api.called())
}

func TestPlayerUnreachable(t *testing.T) {
	t.Parallel()

	p, _ := newFakeAPI(t, nil)
	p.baseURL = "http://127.0.0.1:1"
	require.Error(t, p.play(t.Context()))
}
