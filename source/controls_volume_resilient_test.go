package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type scriptedVolume struct {
	get    float64
	getErr error
	setErr error
	sets   int
}

func (s *scriptedVolume) Backend() string { return "scripted" }

func (s *scriptedVolume) CheckWrite(ctx context.Context) error {
	_, err := s.GetVolume(ctx)
	return err
}

func (s *scriptedVolume) GetVolume(context.Context) (float64, error) {
	if s.getErr != nil {
		return 0, s.getErr
	}
	return s.get, nil
}

func (s *scriptedVolume) SetVolume(_ context.Context, pct int) (float64, error) {
	s.sets++
	if s.setErr != nil {
		return 0, s.setErr
	}
	return float64(pct), nil
}

func TestResilientVolumeFallsBackWhileLunaFailsAndRecovers(t *testing.T) {
	previous := lunaVolumeRetryAfter
	lunaVolumeRetryAfter = time.Millisecond
	defer func() { lunaVolumeRetryAfter = previous }()

	luna := &scriptedVolume{get: 77, getErr: errors.New("not ready"), setErr: errors.New("not ready")}
	mixer := &scriptedVolume{get: 40}
	owner := &resilientVolume{luna: luna, fallback: mixer}

	if owner.Backend() != "scripted" {
		t.Fatalf("backend changed: %q", owner.Backend())
	}
	if got, err := owner.GetVolume(context.Background()); err != nil || got != 40 {
		t.Fatalf("fallback read: %v, %v", got, err)
	}
	if got, err := owner.SetVolume(context.Background(), 55); err != nil || got != 55 {
		t.Fatalf("fallback write: %v, %v", got, err)
	}
	if mixer.sets != 1 {
		t.Fatalf("mixer writes: %d", mixer.sets)
	}

	// After the cooldown the same object must try Luna again, not stay on ALSA
	// for the rest of the boot.
	time.Sleep(3 * time.Millisecond)
	luna.getErr, luna.setErr = nil, nil
	if got, err := owner.GetVolume(context.Background()); err != nil || got != 77 {
		t.Fatalf("Luna recovery read: %v, %v", got, err)
	}
	if got, err := owner.SetVolume(context.Background(), 30); err != nil || got != 30 {
		t.Fatalf("Luna recovery write: %v, %v", got, err)
	}
	if luna.sets != 1 || mixer.sets != 1 {
		t.Fatalf("recovery used the mixer: luna=%d mixer=%d", luna.sets, mixer.sets)
	}
}

func TestResilientVolumeWithoutMixerReturnsLunaError(t *testing.T) {
	luna := &scriptedVolume{getErr: errors.New("no volume")}
	owner := &resilientVolume{luna: luna}
	if _, err := owner.GetVolume(context.Background()); err == nil {
		t.Fatal("Luna failure was hidden without a fallback")
	}
}
