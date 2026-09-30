package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const apiURL = "https://api.spotify.com/v1"

// apiError is a non-2xx answer from the Web API. Reason carries Spotify's
// machine-readable cause, e.g. NO_ACTIVE_DEVICE or PREMIUM_REQUIRED.
type apiError struct {
	Status     int
	Reason     string
	Message    string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	switch {
	case e.Reason == "NO_ACTIVE_DEVICE":
		return "no active Spotify device: start playing in any Spotify app first"
	case e.Reason == "PREMIUM_REQUIRED":
		return "controlling playback requires Spotify Premium"
	case e.Status == http.StatusTooManyRequests:
		return fmt.Sprintf("rate limited by Spotify, retry after %s", e.RetryAfter)
	}
	return fmt.Sprintf("spotify API: %d %s", e.Status, e.Message)
}

type playbackState struct {
	IsPlaying bool `json:"is_playing"`
	Device    struct {
		VolumePercent *int `json:"volume_percent"`
	} `json:"device"`
}

// player is a small client for the /me/player endpoints of the Web API.
type player struct {
	baseURL string
	client  *http.Client
	tokens  *tokens
}

func (p *player) state(ctx context.Context) (playbackState, error) {
	body, status, err := p.call(ctx, http.MethodGet, "/me/player", nil)
	if err != nil {
		return playbackState{}, fmt.Errorf("spotify.state: %w", err)
	}
	// 204 means nothing is playing on any of the user's devices.
	if status == http.StatusNoContent {
		return playbackState{}, fmt.Errorf("spotify.state: %w", &apiError{Status: status, Reason: "NO_ACTIVE_DEVICE"})
	}
	var st playbackState
	if err := json.Unmarshal(body, &st); err != nil {
		return playbackState{}, fmt.Errorf("spotify.state: %w", err)
	}
	return st, nil
}

func (p *player) volume(ctx context.Context) (int, error) {
	st, err := p.state(ctx)
	if err != nil {
		return 0, err
	}
	if st.Device.VolumePercent == nil {
		return 0, errors.New("spotify.volume: the active device does not support volume control")
	}
	return *st.Device.VolumePercent, nil
}

func (p *player) command(ctx context.Context, method, path string, query url.Values) error {
	if _, _, err := p.call(ctx, method, path, query); err != nil {
		return fmt.Errorf("spotify.command %s: %w", path, err)
	}
	return nil
}

func (p *player) play(ctx context.Context) error {
	return p.command(ctx, http.MethodPut, "/me/player/play", nil)
}

func (p *player) pause(ctx context.Context) error {
	return p.command(ctx, http.MethodPut, "/me/player/pause", nil)
}

func (p *player) next(ctx context.Context) error {
	return p.command(ctx, http.MethodPost, "/me/player/next", nil)
}

func (p *player) previous(ctx context.Context) error {
	return p.command(ctx, http.MethodPost, "/me/player/previous", nil)
}

func (p *player) setVolume(ctx context.Context, percent int) error {
	return p.command(ctx, http.MethodPut, "/me/player/volume", url.Values{"volume_percent": {strconv.Itoa(percent)}})
}

func (p *player) call(ctx context.Context, method, path string, query url.Values) ([]byte, int, error) {
	token, err := p.tokens.accessToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	target := p.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("spotify.call: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("spotify.call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("spotify.call: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, resp.StatusCode, nil
	}

	if resp.StatusCode == http.StatusUnauthorized {
		p.tokens.invalidate()
	}
	apiErr := &apiError{Status: resp.StatusCode, Message: resp.Status}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error.Message != "" {
		apiErr.Message = payload.Error.Message
		apiErr.Reason = payload.Error.Reason
	}
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		apiErr.RetryAfter = time.Duration(secs) * time.Second
	}
	return nil, resp.StatusCode, apiErr
}
