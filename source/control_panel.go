package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"station-airplay/internal/panelrpc"
)

const panelSocket = "/run/station-airplay/control.sock"

// The only listener is a mode-0600 Unix socket. Luna proxies these fixed routes
// behind its existing browser guard; the receiver exposes no new LAN port.
func startControlPanel(ctx context.Context, c *sharedController, path, socket string) (func(), error) {
	if path == "" {
		return func() {}, nil
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(socket, 0600); err != nil {
		l.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	var settingsMu sync.Mutex
	respond := func(w http.ResponseWriter, code int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(value)
	}
	fail := func(w http.ResponseWriter, err error) { respond(w, 400, map[string]any{"error": err.Error()}) }
	mux.HandleFunc("/settings", func(w http.ResponseWriter, r *http.Request) {
		settingsMu.Lock()
		defer settingsMu.Unlock()
		if r.Method == "GET" {
			cfg, err := loadControls(path)
			if err != nil {
				fail(w, err)
				return
			}
			respond(w, 200, cfg)
			return
		}
		if r.Method != "PUT" {
			w.WriteHeader(405)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 16385))
		if err != nil {
			fail(w, err)
			return
		}
		before, err := loadControls(path)
		if err != nil {
			fail(w, err)
			return
		}
		if err = savePanelSettings(path, data); err != nil {
			fail(w, err)
			return
		}
		next, err := loadControls(path)
		if err != nil {
			fail(w, err)
			return
		}
		if next != nil {
			if err = c.applyPanelSettings(*next); err != nil {
				fail(w, err)
				return
			}
		}
		// Colours, brightness, points, the black point and the equalizer layout
		// are read every frame. The audio equalizer and the timing mode change
		// the receiver command line, so those still need a restart.
		restart := before == nil || next == nil ||
			before.TimingMode != next.TimingMode ||
			!reflect.DeepEqual(before.Equalizer, next.Equalizer)
		respond(w, 200, map[string]any{"saved": true, "restart_required": restart})
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		live := c.output.snapshot(time.Now())
		zones := []map[string]any{{"id": "artwork", "count": c.ledCount, "supported": c.cfg.LEDs && c.ledCount > 0}}
		if orionDevice(c.device) {
			zones = []map[string]any{}
		}
		respond(w, 200, map[string]any{"live": live, "preview": c.preview.status(time.Now()), "led_count": c.ledCount,
			"model": c.device.Model, "defaults": panelDefaults(c.device),
			"led_level":       currentLEDLevel(c.spectrum, spectrumLEDLevel(c.cfg)),
			"zones":           zones,
			"equalizer_scope": "AirPlay only", "settings_apply": "live", "timing_modes": []string{timingSynchronized, timingFastAudio}})
	})
	mux.HandleFunc("/preview", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" && r.Method != "DELETE" {
			w.WriteHeader(405)
			return
		}
		var request struct {
			ID     string   `json:"id"`
			Colors []string `json:"colors"`
		}
		data, readErr := io.ReadAll(io.LimitReader(r.Body, 8193))
		if readErr != nil {
			fail(w, readErr)
			return
		}
		if len(data) > 8192 {
			respond(w, 413, map[string]string{"error": "preview exceeds 8 KiB"})
			return
		}
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err := d.Decode(&request); err != nil {
			fail(w, err)
			return
		}
		if err := d.Decode(new(any)); err != io.EOF {
			fail(w, errors.New("trailing preview data"))
			return
		}
		if r.Method == "DELETE" {
			if err := c.preview.stop(request.ID); err != nil {
				fail(w, err)
				return
			}
			respond(w, 200, map[string]bool{"stopped": true})
			return
		}
		id, err := c.preview.acquire(request.ID, request.Colors, c.ledCount, time.Now())
		if err != nil {
			fail(w, err)
			return
		}
		sent := append([]string(nil), request.Colors...)
		if c.spectrum != nil {
			sent = c.spectrum.colorize(sent)
		}
		respond(w, 200, map[string]any{"id": id, "ttl_ms": previewTTL.Milliseconds(), "sent": sent})
	})
	mux.HandleFunc("/presets", panelPresets(filepath.Join(filepath.Dir(path), "airplay-panel-presets.json")))
	broker := panelrpc.New(mux)
	mux.Handle("/rpc", broker)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second}
	go func() { _ = server.Serve(l) }()
	stop := func() { _ = server.Close(); _ = os.Remove(socket) }
	go func() { <-ctx.Done(); _ = server.Close() }()
	return stop, nil
}

// Model tuning is supplied by the same backend functions as the renderer, not
// a private device list or a second set of guesses embedded in the browser.
func panelDefaults(d yamDevice) controlsConfig {
	gain, profile, _, _ := spectrumDefaults(d, controlsConfig{})
	floor, _, saturation := spectrumComfort(d)
	on := d.Model != "yandexmini"
	brightness := 1.0
	if d.Model == "yandexmini_2" || d.Model == "plum" || d.Model == "yandexplum" {
		brightness = 1.25
	}
	points := defaultSpectrumPoints()
	return controlsConfig{TimingMode: timingFastAudio, Spectrum: &on, SpectrumGainR: gain[0], SpectrumGainG: gain[1], SpectrumGainB: gain[2],
		SpectrumBrightness: brightness, SpectrumSaturation: saturation, SpectrumBlackFloor: &floor, SpectrumBlackColor: "#808080", LEDLevel: 200, LEDColorProfile: profile,
		SpectrumPoints: &points}
}

func savePanelSettings(path string, data []byte) error {
	if len(data) > 16384 {
		return errors.New("settings exceed 16 KiB")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".panel-settings-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if _, err = loadControls(f.Name()); err != nil {
		return fmt.Errorf("settings rejected: %w", err)
	}
	if old, readErr := os.ReadFile(path); readErr == nil {
		backup, err := os.CreateTemp(filepath.Dir(path), ".panel-backup-")
		if err != nil {
			return err
		}
		defer os.Remove(backup.Name())
		_, writeErr := backup.Write(old)
		if writeErr == nil {
			writeErr = backup.Sync()
		}
		closeErr := backup.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err = os.Rename(backup.Name(), path+".panel-backup"); err != nil {
			return err
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	return os.Rename(f.Name(), path)
}
