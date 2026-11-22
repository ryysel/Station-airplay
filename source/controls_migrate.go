package main

// Existing installs keep their airplay-controls.json across upgrades, so a
// release that enables a new feature would leave it off forever. The launcher
// only seeds a missing file; this migration updates defaults once per release,
// but only where the value still equals the old default. A setting the owner
// changed by hand is never overwritten.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// controlsDefaultsVersion is bumped whenever a release changes the seeded
// defaults in a way existing installs should pick up. Missing in the file
// means version 1 (2.1.4 and older).
const controlsDefaultsVersion = 6

type defaultsRule struct {
	version  int
	model    string
	field    string
	from, to any
}

// rules are matched against the Luna model. `from` is the value the previous
// release seeded; if the owner changed the field, the rule leaves it alone.
var controlsDefaultsRules = []defaultsRule{
	// 2.1.5: Station 2 got a framebuffer ring backend and artwork spectrum.
	// LEDs were inert on this model before, so false is the shipped default.
	{2, "yandexstation_2", "spectrum", false, true},
	{2, "yandexstation_2", "leds", false, true},
	// 2.1.5: the original Mini got the full shared-controls bridge and its
	// six-zone logo. Before that release the package shipped these flags off.
	// `alice_button` stays off: the host does not advertise that transport.
	{3, "yandexmini", "voice", false, true},
	{3, "yandexmini", "ducking", false, true},
	{3, "yandexmini", "leds", false, true},
	{3, "yandexmini", "native_player", false, true},
	{3, "yandexmini", "spectrum", true, false},
	// 2.1.12: the Station Lite firmware does not apply the Quasar or ALSA level
	// to the dmix path AirPlay uses, so forwarding the sender slider to the
	// station had no audible effect. shairport scales the stream itself now;
	// the station keeps its own volume for Alice and the buttons.
	{4, "yandexmicro", "native_volume", true, false},
	// 2.1.18: the original Mini's logo inherited the Lite 2 white balance,
	// which turns purple artwork blue. The six-zone logo is neutral.
	{5, "yandexmini", "spectrum_gain_r", 0.7, 1},
	{5, "yandexmini", "spectrum_gain_g", 0.7, 1},
	{5, "yandexmini", "spectrum_gain_b", 1.85, 1},
	// Station 3: the generic session fill stuck the ring teal for the whole
	// AirPlay session. Alice keeps the ring; existing installs that still
	// have the seeded true are brought back to false.
	{6, "yandexstation_3", "leds", true, false},
	{6, "orion", "leds", true, false},
}

// migrateControls stamps the file with the current defaults version and
// applies the model's pending rules. It reloads the file through the strict
// parser, so a migrated config still cannot smuggle unknown fields.
func migrateControls(path, model string, cfg *controlsConfig) (*controlsConfig, error) {
	if path == "" || cfg == nil || cfg.DefaultsVersion >= controlsDefaultsVersion {
		return cfg, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(data, &raw); err != nil {
		return cfg, err
	}
	var changes []string
	for _, rule := range controlsDefaultsRules {
		if rule.version <= cfg.DefaultsVersion {
			continue
		}
		if rule.model != "*" && rule.model != model {
			continue
		}
		value, ok := raw[rule.field]
		if !ok {
			continue
		}
		var current any
		if err = json.Unmarshal(value, &current); err != nil {
			continue
		}
		if !reflect.DeepEqual(current, rule.from) {
			continue
		}
		if raw[rule.field], err = json.Marshal(rule.to); err != nil {
			return cfg, err
		}
		changes = append(changes, fmt.Sprintf("%s %v -> %v", rule.field, rule.from, rule.to))
	}
	if raw["defaults_version"], err = json.Marshal(controlsDefaultsVersion); err != nil {
		return cfg, err
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return cfg, err
	}
	out = append(out, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".airplay-controls-")
	if err != nil {
		return cfg, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return cfg, err
	}
	if _, err = tmp.Write(out); err != nil {
		tmp.Close()
		return cfg, err
	}
	if err = tmp.Close(); err != nil {
		return cfg, err
	}
	if err = os.Rename(name, path); err != nil {
		return cfg, err
	}
	migrated, err := loadControls(path)
	if err != nil {
		return cfg, err
	}
	if len(changes) > 0 {
		fmt.Fprintf(os.Stderr, "controls config: defaults %d -> %d for %s: %s\n",
			cfg.DefaultsVersion, controlsDefaultsVersion, model, strings.Join(changes, ", "))
	} else {
		fmt.Fprintf(os.Stderr, "controls config: defaults %d -> %d for %s\n",
			cfg.DefaultsVersion, controlsDefaultsVersion, model)
	}
	return migrated, nil
}
