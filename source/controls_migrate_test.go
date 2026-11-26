package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeControlsFile(t *testing.T, body string) (string, *controlsConfig) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "airplay-controls.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadControls(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, cfg
}

func TestMigrateControlsStation2TurnsOnRingFeatures(t *testing.T) {
	path, cfg := writeControlsFile(t, `{
  "voice": true,
  "native_volume": true,
  "ducking": true,
  "leds": false,
  "led_level": 200,
  "native_player": true,
  "alice_button": true,
  "spectrum": false
}`)
	out, err := migrateControls(path, "yandexstation_2", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !out.LEDs {
		t.Fatal("leds stayed off on Station 2")
	}
	if out.Spectrum == nil || !*out.Spectrum {
		t.Fatal("spectrum stayed off on Station 2")
	}
	if out.DefaultsVersion != controlsDefaultsVersion {
		t.Fatalf("defaults version = %d", out.DefaultsVersion)
	}
	if !out.Voice || !out.NativeVolume || !out.Ducking || !out.NativePlayer || !out.AliceButton {
		t.Fatalf("unrelated flags changed: %+v", out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), fmt.Sprintf(`"defaults_version": %d`, controlsDefaultsVersion)) {
		t.Fatalf("file was not stamped: %s", data)
	}
}

func TestMigrateControlsKeepsChangedValue(t *testing.T) {
	path, cfg := writeControlsFile(t, `{"spectrum": true, "leds": false}`)
	out, err := migrateControls(path, "yandexstation_2", cfg)
	if err != nil {
		t.Fatal(err)
	}
	// spectrum already differs from the old default and must stay as it is;
	// leds still equals the old default, so the new feature is enabled.
	if out.Spectrum == nil || !*out.Spectrum {
		t.Fatal("spectrum was rewritten")
	}
	if !out.LEDs {
		t.Fatal("leds default was not upgraded")
	}
}

func TestMigrateControlsOtherModelsKeepDefaults(t *testing.T) {
	path, cfg := writeControlsFile(t, `{"leds": false, "spectrum": false}`)
	out, err := migrateControls(path, "yandexmini_2", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.LEDs {
		t.Fatal("Mini 2 LEDs were enabled by a Station 2 rule")
	}
	if out.Spectrum == nil || *out.Spectrum {
		t.Fatal("Mini 2 spectrum was changed")
	}
	if out.DefaultsVersion != controlsDefaultsVersion {
		t.Fatalf("defaults version = %d", out.DefaultsVersion)
	}
}

func TestMigrateControlsMiniGetsFullControls(t *testing.T) {
	path, cfg := writeControlsFile(t, `{
  "voice": false,
  "native_volume": true,
  "ducking": false,
  "leds": false,
  "led_level": 200,
  "native_player": false,
  "alice_button": false,
  "spectrum": true
}`)
	out, err := migrateControls(path, "yandexmini", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Voice || !out.Ducking || !out.LEDs || !out.NativePlayer {
		t.Fatalf("Mini controls stayed off: %+v", out)
	}
	if out.Spectrum == nil || *out.Spectrum {
		t.Fatal("spectrum stayed on: the logo needs a solid artwork frame")
	}
	if out.AliceButton {
		t.Fatal("Alice button was enabled without a host transport")
	}
	if !out.NativeVolume {
		t.Fatal("native volume changed")
	}
}

func TestMigrateControlsLiteScalesInTheReceiver(t *testing.T) {
	// 2.1.11 and older forwarded the sender slider to the station, where this
	// firmware never applied it to the AirPlay dmix path. The new default lets
	// shairport scale the stream, so only an untouched `true` is replaced.
	path, cfg := writeControlsFile(t, `{
  "voice": true,
  "native_volume": true,
  "ducking": true,
  "leds": true,
  "native_player": false,
  "alice_button": false,
  "spectrum": true,
  "defaults_version": 3
}`)
	out, err := migrateControls(path, "yandexmicro", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.NativeVolume {
		t.Fatal("Lite kept forwarding the sender slider to the station")
	}
	if !out.Voice || !out.Ducking || !out.LEDs || out.NativePlayer || out.AliceButton {
		t.Fatalf("unrelated flags changed: %+v", out)
	}
	// Another model with the same file keeps its native volume.
	otherPath, other := writeControlsFile(t, `{"native_volume": true, "defaults_version": 3}`)
	otherOut, err := migrateControls(otherPath, "yandexmini_2", other)
	if err != nil {
		t.Fatal(err)
	}
	if !otherOut.NativeVolume {
		t.Fatal("a Lite rule touched Mini 2")
	}
}

func TestMigrateControlsOrionTurnsOffLEDs(t *testing.T) {
	path, cfg := writeControlsFile(t, `{
  "voice": true,
  "native_volume": true,
  "ducking": true,
  "leds": true,
  "native_player": false,
  "alice_button": false,
  "spectrum": false,
  "defaults_version": 5
}`)
	out, err := migrateControls(path, "yandexstation_3", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.LEDs {
		t.Fatal("Station 3 kept session LED fill")
	}
	orionPath, orionCfg := writeControlsFile(t, `{"leds": true, "defaults_version": 5}`)
	orionOut, err := migrateControls(orionPath, "orion", orionCfg)
	if err != nil {
		t.Fatal(err)
	}
	if orionOut.LEDs {
		t.Fatal("orion kept session LED fill")
	}
	keptPath, kept := writeControlsFile(t, `{"leds": true, "defaults_version": 5}`)
	keptOut, err := migrateControls(keptPath, "yandexstation_2", kept)
	if err != nil {
		t.Fatal(err)
	}
	if !keptOut.LEDs {
		t.Fatal("a Station 3 LED rule touched Station 2")
	}
}

func TestMigrateControlsIsIdempotent(t *testing.T) {
	path, cfg := writeControlsFile(t, `{"leds": false, "spectrum": false}`)
	if _, err := migrateControls(path, "yandexstation_2", cfg); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := loadControls(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrateControls(path, "yandexstation_2", again); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("second migration rewrote the file:\n%s\n%s", first, second)
	}
}

func TestMigrateControlsWithoutFileIsNoop(t *testing.T) {
	if out, err := migrateControls("", "yandexstation_2", &controlsConfig{}); err != nil || out == nil {
		t.Fatalf("empty path: %v %v", out, err)
	}
}
