package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stateAt(level int) http.HandlerFunc {
	return respond(http.StatusOK, fmt.Sprintf(`{"is_playing":true,"device":{"volume_percent":%d}}`, level))
}

func newTestVolume(t *testing.T, routes map[string]http.HandlerFunc) (*volume, *fakeAPI) {
	t.Helper()
	p, api := newFakeAPI(t, routes)
	v := newVolume(p, slog.New(slog.DiscardHandler))
	v.interval = 30 * time.Millisecond
	v.minBackoff = time.Millisecond
	return v, api
}

func runVolume(t *testing.T, v *volume) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		v.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestVolumeCoalescesTicks(t *testing.T) {
	t.Parallel()

	v, api := newTestVolume(t, map[string]http.HandlerFunc{"GET /me/player": stateAt(40)})
	for range 5 {
		require.NoError(t, v.step(t.Context(), 5))
	}
	// Started after the ticks so the test does not depend on them landing
	// inside the coalescing window; wake is buffered, so run still sees them.
	runVolume(t, v)

	require.Eventually(t, func() bool { return api.count("PUT") == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(3 * v.interval)
	assert.Equal(t, []string{"GET /me/player", "PUT /me/player/volume?volume_percent=65"}, api.called())
}

func TestVolumeClamps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		level int
		delta int
		want  int
	}{
		{name: "up to 100", level: 98, delta: 5, want: 100},
		{name: "down to 0", level: 2, delta: -5, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v, _ := newTestVolume(t, map[string]http.HandlerFunc{"GET /me/player": stateAt(tt.level)})
			require.NoError(t, v.step(t.Context(), tt.delta))
			assert.Equal(t, tt.want, v.target)
		})
	}
}

func TestVolumeToggleMute(t *testing.T) {
	t.Parallel()

	v, _ := newTestVolume(t, map[string]http.HandlerFunc{"GET /me/player": stateAt(40)})
	require.NoError(t, v.toggleMute(t.Context(), 30))
	assert.Equal(t, 0, v.target)
	require.NoError(t, v.toggleMute(t.Context(), 30))
	assert.Equal(t, 40, v.target, "unmuting restores the previous level")
}

func TestVolumeUnmuteWithoutPreviousLevel(t *testing.T) {
	t.Parallel()

	v, _ := newTestVolume(t, map[string]http.HandlerFunc{"GET /me/player": stateAt(0)})
	require.NoError(t, v.toggleMute(t.Context(), 30))
	assert.Equal(t, 30, v.target)
}

func TestVolumeRereadsStaleLevel(t *testing.T) {
	t.Parallel()

	var level atomic.Int32
	level.Store(40)
	v, api := newTestVolume(t, map[string]http.HandlerFunc{
		"GET /me/player": func(w http.ResponseWriter, r *http.Request) { stateAt(int(level.Load()))(w, r) },
	})
	now := time.Unix(1000, 0)
	v.now = func() time.Time { return now }

	require.NoError(t, v.step(t.Context(), 5))
	v.pending = false // as if run flushed it
	require.NoError(t, v.step(t.Context(), 5))
	assert.Equal(t, 50, v.target)
	assert.Equal(t, 1, api.count("GET"), "a recent level is trusted")

	// Changed in a Spotify app while the encoder was idle.
	level.Store(10)
	now = now.Add(v.staleAfter)
	v.pending = false
	require.NoError(t, v.step(t.Context(), 5))
	assert.Equal(t, 15, v.target)
	assert.Equal(t, 2, api.count("GET"))
}

func TestVolumeStaleReadKeepsUnflushedTarget(t *testing.T) {
	t.Parallel()

	v, _ := newTestVolume(t, map[string]http.HandlerFunc{"GET /me/player": stateAt(40)})
	now := time.Unix(1000, 0)
	v.now = func() time.Time { return now }

	require.NoError(t, v.step(t.Context(), 20))
	now = now.Add(v.staleAfter)
	// The 60 not sent yet is newer than the 40 Spotify reports.
	require.NoError(t, v.step(t.Context(), 5))
	assert.Equal(t, 65, v.target)
}

func TestVolumeStepWithoutDevice(t *testing.T) {
	t.Parallel()

	v, api := newTestVolume(t, map[string]http.HandlerFunc{"GET /me/player": respond(http.StatusNoContent, "")})
	require.ErrorContains(t, v.step(t.Context(), 5), "no active Spotify device")
	assert.False(t, v.known)
	assert.Zero(t, api.count("PUT"))
}

func TestVolumeRetriesAfterRateLimit(t *testing.T) {
	t.Parallel()

	var puts atomic.Int32
	v, api := newTestVolume(t, map[string]http.HandlerFunc{
		"GET /me/player": stateAt(40),
		"PUT /me/player/volume": func(w http.ResponseWriter, _ *http.Request) {
			if puts.Add(1) == 1 {
				// No Retry-After: run still backs off by minBackoff.
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		},
	})
	runVolume(t, v)

	require.NoError(t, v.step(t.Context(), 5))
	require.Eventually(t, func() bool { return api.count("PUT") == 2 }, time.Second, 5*time.Millisecond)
	assert.Equal(t, []string{
		"GET /me/player",
		"PUT /me/player/volume?volume_percent=45",
		"PUT /me/player/volume?volume_percent=45",
	}, api.called())
}

func TestVolumeResyncsAfterFailedFlush(t *testing.T) {
	t.Parallel()

	v, api := newTestVolume(t, map[string]http.HandlerFunc{
		"GET /me/player":        stateAt(40),
		"PUT /me/player/volume": respond(http.StatusInternalServerError, ""),
	})
	runVolume(t, v)

	require.NoError(t, v.step(t.Context(), 5))
	require.Eventually(t, func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		return !v.known
	}, time.Second, 5*time.Millisecond)
	require.NoError(t, v.step(t.Context(), 5))
	assert.Equal(t, 2, api.count("GET"), "the next tick reads the level again")
}

func TestVolumeRunStopsWhileWaiting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		interval time.Duration
		handler  http.HandlerFunc
	}{
		{name: "in the coalescing window", interval: time.Hour, handler: respond(http.StatusNoContent, "")},
		{
			name:     "in a rate limit backoff",
			interval: time.Millisecond,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v, api := newTestVolume(t, map[string]http.HandlerFunc{"GET /me/player": stateAt(40), "PUT /me/player/volume": tt.handler})
			v.interval = tt.interval
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				v.run(ctx)
				close(done)
			}()

			require.NoError(t, v.step(t.Context(), 5))
			if tt.interval == time.Millisecond {
				require.Eventually(t, func() bool { return api.count("PUT") == 1 }, time.Second, 5*time.Millisecond)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("run did not stop")
			}
		})
	}
}
