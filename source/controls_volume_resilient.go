package main

// Luna stays the station-volume owner even when a cold boot makes its
// /api/volume endpoint fail or answer slowly.
//
// The direct ALSA mixer is only a per-call fallback. Writing it alone leaves
// Quasar's logical level untouched: that is the level the hardware buttons, the
// ring, Alice and (on boards with a controlled amplifier) the audible output
// follow. On Station 2 and Lite 2 the sender's slider then looked dead even
// though `amixer set Master` reported success. The old one-shot startup probe
// made that permanent: at T+17 s the first Luna write can exceed 800 ms while
// Quasar and mediad are still publishing their IPC, so the plugin switched to
// ALSA for the whole boot and never re-probed.
//
// The fallback stays available for a host whose Luna has no volume endpoint at
// all, but it is retried lazily: three seconds after a failure the next call
// tries Luna again, so a station recovers by itself once boot settles.

import (
	"context"
	"fmt"
	"sync"
	"time"
)

var lunaVolumeRetryAfter = 3 * time.Second

type resilientVolume struct {
	luna     volumeControl
	fallback volumeControl

	mu        sync.Mutex
	downUntil time.Time
	reported  bool
}

func (v *resilientVolume) Backend() string { return v.luna.Backend() }

func (v *resilientVolume) coolingDown() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.fallback != nil && time.Now().Before(v.downUntil)
}

func (v *resilientVolume) markDown(operation string, err error) {
	v.mu.Lock()
	v.downUntil = time.Now().Add(lunaVolumeRetryAfter)
	first := !v.reported
	v.reported = true
	v.mu.Unlock()
	if first {
		fmt.Printf("station volume: Luna %s is not ready: %v; using the direct ALSA mixer until it recovers\n",
			operation, err)
	}
}

func (v *resilientVolume) markUp() {
	v.mu.Lock()
	recovered := v.reported
	v.reported = false
	v.downUntil = time.Time{}
	v.mu.Unlock()
	if recovered {
		fmt.Println("station volume: Luna /api/volume is available again")
	}
}

// via performs one operation with Luna first and the direct mixer only while
// Luna is failing. A failed fallback keeps the Luna error as the cause.
func (v *resilientVolume) via(
	ctx context.Context,
	operation string,
	luna func(context.Context) (float64, error),
	mixer func(context.Context) (float64, error),
) (float64, error) {
	var lunaErr error
	if !v.coolingDown() {
		value, err := luna(ctx)
		if err == nil {
			v.markUp()
			return value, nil
		}
		lunaErr = err
		v.markDown(operation, err)
	}
	if mixer != nil {
		value, err := mixer(ctx)
		if err == nil {
			return value, nil
		}
		if lunaErr == nil {
			lunaErr = err
		}
	}
	// The cooldown only exists to keep a stalled Luna endpoint off the fast
	// path. If the mixer cannot answer either, try Luna once more rather than
	// leaving the station without any volume control.
	if v.coolingDown() {
		value, err := luna(ctx)
		if err == nil {
			v.markUp()
			return value, nil
		}
		lunaErr = err
	}
	return 0, lunaErr
}

func (v *resilientVolume) GetVolume(ctx context.Context) (float64, error) {
	mixer := (func(context.Context) (float64, error))(nil)
	if v.fallback != nil {
		mixer = v.fallback.GetVolume
	}
	return v.via(ctx, "read", v.luna.GetVolume, mixer)
}

func (v *resilientVolume) SetVolume(ctx context.Context, pct int) (float64, error) {
	mixer := (func(context.Context) (float64, error))(nil)
	if v.fallback != nil {
		mixer = func(ctx context.Context) (float64, error) {
			return v.fallback.SetVolume(ctx, pct)
		}
	}
	return v.via(ctx, "write", func(ctx context.Context) (float64, error) {
		return v.luna.SetVolume(ctx, pct)
	}, mixer)
}

func (v *resilientVolume) CheckWrite(ctx context.Context) error {
	_, err := v.GetVolume(ctx)
	return err
}
