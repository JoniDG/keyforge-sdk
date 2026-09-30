package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// volume coalesces encoder ticks. Handlers only move a target in memory; run
// sends it to Spotify at most once per interval, so a fast spin of the
// encoder costs a couple of requests instead of one per tick, which would
// quickly hit Spotify's rate limit.
type volume struct {
	player   *player
	logger   *slog.Logger
	interval time.Duration
	// staleAfter is how long the cached level is trusted without a tick: past
	// it, the next tick reads the level again in case it changed elsewhere.
	staleAfter time.Duration
	minBackoff time.Duration
	now        func() time.Time

	mu        sync.Mutex
	known     bool
	target    int
	restore   int  // level to go back to when unmuting
	pending   bool // target has changes Spotify has not acknowledged yet
	lastTouch time.Time
	wake      chan struct{}
}

func newVolume(p *player, logger *slog.Logger) *volume {
	return &volume{
		player:     p,
		logger:     logger,
		interval:   150 * time.Millisecond,
		staleAfter: 5 * time.Second,
		minBackoff: time.Second,
		now:        time.Now,
		wake:       make(chan struct{}, 1),
	}
}

func (v *volume) step(ctx context.Context, delta int) error {
	return v.adjust(ctx, func(cur int) int { return cur + delta })
}

// toggleMute sets the volume to 0, or back to the level it had before. When
// muted from elsewhere there is no level to go back to, so it uses fallback.
func (v *volume) toggleMute(ctx context.Context, fallback int) error {
	return v.adjust(ctx, func(cur int) int {
		if cur > 0 {
			v.restore = cur
			return 0
		}
		if v.restore > 0 {
			return v.restore
		}
		return fallback
	})
}

// adjust applies f to the target level (under v.mu) and wakes run.
func (v *volume) adjust(ctx context.Context, f func(cur int) int) error {
	v.mu.Lock()
	fresh := v.known && v.now().Sub(v.lastTouch) < v.staleAfter
	v.mu.Unlock()

	// Handlers run one at a time, so no other tick can refresh the level
	// while this one reads it without holding the lock.
	var current int
	if !fresh {
		level, err := v.player.volume(ctx)
		if err != nil {
			return err
		}
		current = level
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if !fresh && !v.pending {
		v.target = current
	}
	v.known = true
	v.target = min(max(f(v.target), 0), 100)
	v.lastTouch = v.now()
	v.pending = true
	v.signal()
	return nil
}

func (v *volume) signal() {
	select {
	case v.wake <- struct{}{}:
	default: // already woken; run picks up the latest target
	}
}

// run flushes the target level until ctx is done.
func (v *volume) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-v.wake:
		}
		// Wait for the ticks that follow this one to land in the same request.
		if !sleep(ctx, v.interval) {
			return
		}
		v.mu.Lock()
		target := v.target
		// adjust signals under v.mu, so any wake-up queued by now is for a
		// tick this target already includes.
		select {
		case <-v.wake:
		default:
		}
		v.mu.Unlock()

		err := v.player.setVolume(ctx, target)
		if err == nil {
			v.mu.Lock()
			// A tick that landed during the request is still to be sent.
			if v.target == target {
				v.pending = false
			}
			v.mu.Unlock()
			continue
		}
		v.logger.Warn("set volume", "error", err)
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests {
			// Without a usable Retry-After, still back off instead of retrying
			// every interval while the limit lasts.
			if !sleep(ctx, max(apiErr.RetryAfter, v.minBackoff)) {
				return
			}
			v.mu.Lock()
			v.signal()
			v.mu.Unlock()
			continue
		}
		// The level Spotify has is unknown now: read it again on the next tick.
		v.mu.Lock()
		v.known = false
		v.pending = false
		v.mu.Unlock()
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
