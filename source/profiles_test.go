package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonDeathCancelsService(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchParent(ctx, cancel, 42, func() int { return 1 })
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("orphaned plugin did not stop")
	}
}

func TestReceiverLogsStayBounded(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "receiver.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := &boundedLog{f: f}
	large := []byte(strings.Repeat("x", 300*1024))
	if n, err := w.Write(large); err != nil || n != len(large) {
		t.Fatalf("write contract %d %v", n, err)
	}
	if _, err := w.Write([]byte("new session")); err != nil {
		t.Fatal(err)
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 128*1024 {
		t.Fatalf("unbounded RAM log: %d", fi.Size())
	}
}

func TestSharedProfilesKeepProvenAudioContract(t *testing.T) {
	for _, tc := range []struct{ model, pcm, abi, icon string }{
		{"yandexstation_2", "plug:dmix_spk", "armhf", "AudioAccessory1,1"},
		{"yandexstation_3", "speakers", "armhf", "AudioAccessory5,1"},
		{"yandexmini_2", "default", "armhf", "AudioAccessory5,1"},
		{"yandexmini_3_pro", "speakers", "aarch64", "AudioAccessory5,1"},
		{"yandexmini3", "speakers", "armhf", "AudioAccessory5,1"},
		{"bergamot", "speakers", "armhf", "AudioAccessory5,1"},
		{"yandexmicro", "plug:dmix_spk", "armhf", "AudioAccessory5,1"},
		{"yandexmini", "softvol", "armhf", "AudioAccessory5,1"},
		{"YandexStationMini", "softvol", "armhf", "AudioAccessory5,1"},
		{"mango", "default", "armhf", "AudioAccessory5,1"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			p, ok := profileFor(tc.model)
			if !ok {
				t.Fatal("missing shared profile")
			}
			if p.pcm != tc.pcm || p.receiverABI != tc.abi || !p.quasarCoexist {
				t.Fatalf("wrong ABI/audio contract: %+v", p)
			}
			if tc.model == "yandexstation_3" && !p.packagedDBus {
				t.Fatal("Station 3 must use the packaged armhf D-Bus")
			}
			conf := receiverConfig(p)
			port := "port = 5000"
			if tc.model == "yandexstation_3" {
				port = "port = 7000"
			}
			for _, s := range []string{tc.pcm, tc.icon, port, "output_rate = 48000", "output_channels = 2", "session_timeout = 60"} {
				if !strings.Contains(conf, s) {
					t.Fatalf("lost known-working setting %q", s)
				}
			}
			if strings.Contains(conf, "hw:0,0") || strings.Contains(conf, "spk_tee") {
				t.Fatal("exclusive/FIFO audio must not replace shared PCM")
			}
		})
	}
	for _, model := range []string{"", "yandexmini_2_unverified", "cucumber", "unknown"} {
		if _, ok := profileFor(model); ok {
			t.Fatalf("model %q guessed instead of rejected", model)
		}
	}
}

// The short backend buffer is independent of presentation-time offset. Default
// profiles retain the owner's fast-audio default on every model.
func TestSharedReceiverKeepsShortBackendBuffer(t *testing.T) {
	for _, model := range []string{"yandexmini", "yandexmini_2", "yandexstation_2", "yandexmidi", "yandexplum", "yandexmicro"} {
		p, ok := profileFor(model)
		if !ok {
			t.Fatal(model)
		}
		conf := receiverConfig(p)
		for _, s := range []string{
			"audio_backend_buffer_desired_length_in_seconds = 0.10;",
			"audio_backend_latency_offset_in_seconds = -1.7;",
		} {
			if !strings.Contains(conf, s) {
				t.Fatalf("%s lacks %q:\n%s", model, s, conf)
			}
		}
	}
}

func TestLight2SharesMixerAndHoldsDisplay(t *testing.T) {
	for _, model := range []string{"yandexplum", "plum"} {
		p, ok := profileFor(model)
		if !ok || !p.ui || p.leds != 3 || p.pcm != "plug:dmix_spk" || p.receiverABI != "armhf" || !p.quasarCoexist || p.airplayModel != "AudioAccessory5,1" {
			t.Fatalf("%s contract: %+v", model, p)
		}
		conf := receiverConfig(p)
		for _, s := range []string{"spk_tee", "port = 5000", "output_rate = 48000", "AudioAccessory5,1", metadataFIFO, "Yandex Station Light 2 AirPlay"} {
			if !strings.Contains(conf, s) {
				t.Fatalf("%s config lost %q", model, s)
			}
		}
		if strings.Contains(conf, "hw:0,0") || strings.Contains(conf, "plug:dmix_spk") {
			t.Fatal("shared mixer must stay in the private asoundrc, not the receiver device string")
		}
		asound := uiAsound(p.pcm, audioFIFO)
		if !strings.Contains(asound, "plug:dmix_spk") || strings.Contains(asound, "hw:0,0") {
			t.Fatalf("tee slave: %s", asound)
		}
	}
}

func TestLiteScalesTheSenderVolumeInTheReceiver(t *testing.T) {
	// The Station Lite firmware acknowledges native VolumeSet and mirrors it to
	// ALSA Master, but neither level reaches the dmix path AirPlay plays on:
	// a direct plug:dmix_spk measurement at Master 100% and 5% was identical.
	// The packaged defaults therefore leave native_volume off, which is what
	// drops ignore_volume_control and lets shairport apply the AirPlay slider.
	// Only the receiver-side path was audible on firmware 0.369.1.46.
	p, ok := profileFor("yandexmicro")
	if !ok || p.pcm != "plug:dmix_spk" || !p.quasarCoexist {
		t.Fatalf("Lite profile: %+v", p)
	}
	conf := receiverControlConfig(p, &controlsConfig{Voice: true, Ducking: true, LEDs: true})
	if strings.Contains(conf, "ignore_volume_control") {
		t.Fatalf("Lite forces the receiver to 0 dB, so the slider is inaudible:\n%s", conf)
	}
	if !strings.Contains(conf, "volume_range_db = 50;") {
		t.Fatalf("Lite lost the receiver-side volume range:\n%s", conf)
	}
	// Guard the mechanism: a profile that does own the station volume still
	// asks the receiver for a flat stream.
	native := receiverControlConfig(p, &controlsConfig{Voice: true, NativeVolume: true})
	if !strings.Contains(native, `ignore_volume_control = "yes"`) {
		t.Fatal("native volume no longer pins the receiver to 0 dB")
	}
}

func TestChildrenCannotInheritYAMTokenOrPlumLibraries(t *testing.T) {
	env := audioEnv([]string{"PATH=/bin", "YAM_API_TOKEN=private", "YAM_API_URL=http://localhost", "LD_LIBRARY_PATH=/plum/lib", "HOME=/plum", "DBUS_SYSTEM_BUS_ADDRESS=unix:path=/system"}, "/run/station-airplay", "/private/bus")
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"YAM_API_", "private\n", "/plum", "/system"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("unsafe inherited environment: %q", joined)
		}
	}
	if !strings.Contains(joined, "DBUS_SYSTEM_BUS_ADDRESS=unix:path=/private/bus") || !strings.Contains(joined, "PATH=/bin") {
		t.Fatal("lost child bus or PATH")
	}
}

func TestStockMDNSHostname(t *testing.T) {
	for model, want := range map[string]string{"yandexplum": "YandexLite-gen2-TEST123", "yandexstation_2": "yandex-station-gen2-TEST123", "yandexmini_3_pro": "Yandex-Station-Mini-3-Pro-TEST123", "yandexmini3": "YandexMini3-TEST123", "bergamot": "YandexMini3-TEST123", "yandexmicro": "yandexmicro-TEST123"} {
		p, _ := profileFor(model)
		got, err := stockHostname(p.mdnsHostPrefix, "root=ubi androidboot.serialno=TEST123 quiet")
		if err != nil || got != want {
			t.Fatalf("hostname %q %v", got, err)
		}
	}
	for _, cmdline := range []string{"", "androidboot.serialno=../bad", "androidboot.serialno=bad\""} {
		if _, err := stockHostname("yandex-station-gen2", cmdline); err == nil {
			t.Fatalf("accepted unsafe serial %q", cmdline)
		}
	}
	// The Mini's maind publishes the Linkplay mv_uuid, not the boot serial.
	p, _ := profileFor("yandexmini")
	if !p.nvramHostID {
		t.Fatal("Mini must take its mDNS host id from Linkplay nvram")
	}
	got, err := nvramHostname(p.mdnsHostPrefix, []byte(`{"Private_PrjName":"YandexStationMini","SSID1":"YandexStationMini_TEST","mv_uuid":"FF00TEST123"}`))
	if err != nil || got != "yandexmini-FF00TEST123" {
		t.Fatalf("nvram hostname %q %v", got, err)
	}
	for _, raw := range []string{"", "{}", `{"mv_uuid":"../bad"}`, `{"mv_uuid":"bad\""}`} {
		if _, err := nvramHostname("yandexmini", []byte(raw)); err == nil {
			t.Fatalf("accepted unsafe nvram %q", raw)
		}
	}
}

func TestMiniUsesFirmwareALSAAndPackagedDBus(t *testing.T) {
	mini, _ := profileFor("yandexmini")
	if got := firmwareALSA(mini); got != "/system/vendor/quasar/libs/libasound.so.2" {
		t.Fatalf("Mini firmware ALSA %q", got)
	}
	bin, args := dbusCommand("/pkg", mini, "dbus-daemon")
	if bin != "/pkg/bin/dbus-daemon" || len(args) != 0 {
		t.Fatalf("Mini D-Bus %s %v", bin, args)
	}
	station2, _ := profileFor("yandexstation_2")
	if got := firmwareALSA(station2); got != "/usr/lib/libasound.so.2" {
		t.Fatalf("Station 2 firmware ALSA changed: %q", got)
	}
	if bin, args := dbusCommand("/pkg", station2, "dbus-send"); bin != "/usr/bin/dbus-send" || len(args) != 0 {
		t.Fatalf("Station 2 must keep firmware D-Bus: %s %v", bin, args)
	}
}

func TestClocklessLiteUsesSharedAudio(t *testing.T) {
	for _, model := range []string{"plum", "yandexplum"} {
		p, ok := profileForDevice(yamDevice{Model: model}, false)
		if !ok || p.pcm != "speakers" || !p.quasarCoexist || p.receiverABI != "armhf" || p.leds != 0 {
			t.Fatalf("clockless Lite must share audio without UI: %+v", p)
		}
		p, ok = profileForDevice(yamDevice{Model: model, Features: []string{"display.clock.segment.v1"}}, false)
		if !ok || p.pcm != "plug:dmix_spk" || p.leds != 3 || !p.ui || !p.quasarCoexist {
			t.Fatal("upstream Light 2 display renderer changed")
		}
	}
	if _, ok := profileForDevice(yamDevice{Model: "unknown"}, false); ok {
		t.Fatal("ambiguous hardware must not be guessed")
	}
}

// Captured YAM reports from the Lite pilot include this clock capability.
// Shared audio must therefore be explicit, not inferred from clock absence.
func TestPlumSharedOptInWithReportedClock(t *testing.T) {
	for _, model := range []string{"plum", "yandexplum"} {
		d := yamDevice{Model: model, Architecture: "aarch64", Features: []string{"display.clock.segment.v1", "display.clock.music.v1", "led.ring.rgb", "led.ring.3", "audio.volume.alsa"}}
		p, ok := profileForDevice(d, true)
		if !ok || p.pcm != "speakers" || !p.quasarCoexist || p.receiverABI != "armhf" || p.leds != 3 || !p.ui {
			t.Fatalf("shared clock profile lost its mixer or display: %+v", p)
		}
		if err := validateControlRenderer(p, &controlsConfig{LEDs: true, Voice: true}); err != nil {
			t.Fatalf("shared clock disabled controls: %v", err)
		}
		conf := receiverControlConfig(p, &controlsConfig{NativeVolume: true})
		if !strings.Contains(conf, `output_device = "spk_tee"`) || !strings.Contains(conf, metadataFIFO) {
			t.Fatal("shared clock is not receiving PCM and metadata")
		}
		legacy, ok := profileForDevice(d, false)
		if !ok || legacy.pcm != "plug:dmix_spk" || legacy.leds != 3 || !legacy.ui || !legacy.quasarCoexist {
			t.Fatal("upstream clock-equipped Light 2 profile changed")
		}
	}
	if _, ok := profileForDevice(yamDevice{Model: "unknown"}, true); ok {
		t.Fatal("shared mode must not bypass the model guard")
	}
}

func TestLiteSharedDisplayRequiresCompleteCapabilities(t *testing.T) {
	for _, features := range [][]string{
		nil,
		{"display.clock.music.v1", "led.ring.rgb", "led.ring.3"},
		{"display.clock.segment.v1", "display.clock.music.v1", "led.ring.3"},
		{"display.clock.segment.v1", "display.clock.music.v1", "led.ring.rgb"},
	} {
		p, ok := profileForDevice(yamDevice{Model: "yandexplum", Features: features}, true)
		if !ok || p.ui || p.pcm != "speakers" {
			t.Fatalf("incomplete display capabilities must retain shared audio: %+v", p)
		}
		if err := validateControlRenderer(p, &controlsConfig{LEDs: true, Voice: true}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLiteMusicClockPolicyRequiresHostCapability(t *testing.T) {
	features := []string{"display.clock.segment.v1", "led.ring.rgb", "led.ring.3"}
	oldHost, _ := profileForDevice(yamDevice{Model: "yandexplum", Features: features}, true)
	if oldHost.ui || oldHost.sharedUI {
		t.Fatal("old host must retain shared audio without taking the clock")
	}
	features = append(features, "display.clock.music.v1")
	newHost, _ := profileForDevice(yamDevice{Model: "yandexplum", Features: features}, true)
	if !newHost.sharedUI || newHost.pcm != "speakers" {
		t.Fatal("new Lite host lost configurable clock or shared mixer")
	}
}

func TestMidiMusicClockRequiresBothHostCapabilities(t *testing.T) {
	features := []string{"display.clock.segment.v1", "display.clock.music.v1"}
	legacy, ok := profileForDevice(yamDevice{Model: "yandexmidi"}, true)
	if !ok || !legacy.ui || legacy.sharedUI {
		t.Fatalf("legacy MIDI host lost its existing renderer: %+v", legacy)
	}
	for _, partial := range [][]string{
		{"display.clock.segment.v1"},
		{"display.clock.music.v1"},
	} {
		p, ok := profileForDevice(yamDevice{Model: "yandexmidi", Features: partial}, true)
		if !ok || !p.ui || p.sharedUI {
			t.Fatalf("partial MIDI clock capabilities changed fallback: %+v", p)
		}
	}
	p, ok := profileForDevice(yamDevice{Model: "yandexmidi", Features: features}, true)
	if !ok || !p.ui || !p.sharedUI || p.pcm != "speakers" || p.outputRate != 96000 || p.leds != 24 || p.digitBase != 1 || len(p.colon) != 0 {
		t.Fatalf("capable MIDI host lost existing audio or display geometry: %+v", p)
	}
}

func TestControlsDoNotSilentlyLoseTheirLoopToUpstreamDisplay(t *testing.T) {
	clock := yamDevice{Model: "yandexplum", Features: []string{"display.clock.segment.v1"}}
	upstream, ok := profileForDevice(clock, false)
	if !ok || !upstream.ui {
		t.Fatal("missing upstream display mode")
	}
	if err := validateControlRenderer(upstream, nil); err != nil {
		t.Fatal(err)
	}
	if err := validateControlRenderer(upstream, &controlsConfig{}); err == nil || !strings.Contains(err.Error(), "--shared-audio") {
		t.Fatalf("display and controls were combined: %v", err)
	}
	audioOnly, ok := profileForDevice(clock, true) // Incomplete ring capabilities must keep audio available.
	if !ok || audioOnly.ui {
		t.Fatal("explicit shared mode retained upstream renderer")
	}
	if err := validateControlRenderer(audioOnly, &controlsConfig{}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"yandexmini_2", "yandexstation_2", "yandexmicro"} {
		p, ok := profileFor(model)
		if !ok {
			t.Fatal(model)
		}
		if err := validateControlRenderer(p, &controlsConfig{}); err != nil {
			t.Fatal(model, err)
		}
	}
	for _, model := range []string{"yandexmini_3_pro"} {
		p, ok := profileFor(model)
		if !ok {
			t.Fatal(model)
		}
		if err := validateControlRenderer(p, &controlsConfig{}); err == nil {
			t.Fatal("unsupported controls enabled", model)
		}
	}
	station3, ok := profileFor("yandexstation_3")
	if !ok {
		t.Fatal("yandexstation_3")
	}
	if err := validateControlRenderer(station3, &controlsConfig{}); err != nil {
		t.Fatal(err)
	}
}

// The original Mini on Luna 0.2.26 advertises speech, commands, LED and
// native player in /media, so its packaged D-Bus bridge carries the same
// shared controls as the other ARMhf profiles. Station 3 uses that same
// gate. Mini 3 Pro stays audio-only: its receiver is the unqualified
// AArch64 build.
func TestOriginalMiniSharesControlsOnPackagedDBus(t *testing.T) {
	full := &controlsConfig{NativeVolume: true, Voice: true, Ducking: true, LEDs: true, NativePlayer: true, AliceButton: true}
	for _, model := range []string{"yandexmini", "YandexStationMini"} {
		p, ok := profileFor(model)
		if !ok {
			t.Fatal(model)
		}
		if !p.sharedControls || !p.packagedDBus {
			t.Fatalf("%s lost the packaged-D-Bus control profile: %+v", model, p)
		}
		if err := validateControlRenderer(p, full); err != nil {
			t.Fatalf("%s rejected shared controls: %v", model, err)
		}
		if p.pcm != "softvol" || !p.quasarCoexist || p.receiverABI != "armhf" || firmwareALSA(p) != "/system/vendor/quasar/libs/libasound.so.2" {
			t.Fatalf("%s audio contract changed: %+v", model, p)
		}
	}
	for _, model := range []string{"yandexmini_3_pro"} {
		p, ok := profileFor(model)
		if !ok {
			t.Fatal(model)
		}
		if p.sharedControls {
			t.Fatalf("%s must stay an audio profile", model)
		}
		if err := validateControlRenderer(p, full); err == nil {
			t.Fatalf("%s accepted unqualified controls", model)
		}
	}
	station3, ok := profileFor("yandexstation_3")
	if !ok || !station3.sharedControls || !station3.packagedDBus {
		t.Fatalf("Station 3 lost the packaged-D-Bus control profile: %+v", station3)
	}
	if err := validateControlRenderer(station3, full); err != nil {
		t.Fatalf("Station 3 rejected shared controls: %v", err)
	}
	if station3.airplayModel != "AudioAccessory5,1" || profileRTSPPort(station3) != 7000 {
		t.Fatalf("Station 3 lost AirPlay 2 identity: %+v", station3)
	}
	conf := receiverConfig(station3)
	if !strings.Contains(conf, "port = 7000") || !strings.Contains(conf, `interface = "wlan0"`) || strings.Contains(conf, `service_type = "classic"`) {
		t.Fatalf("Station 3 conf is not AirPlay 2: %s", conf)
	}
}

func TestMidiUsesSharedMixerAndKeepsDisplay(t *testing.T) {
	p, ok := profileForDevice(yamDevice{Model: "yandexmidi"}, true)
	if !ok || p.pcm != "speakers" || p.receiverABI != "armhf" || !p.ui || p.leds != 24 || p.digitBase != 1 {
		t.Fatalf("Midi must use its shared PCM and existing renderer: %+v", p)
	}
	if p.name != "Yandex Midi AirPlay" || firmwareALSA(p) != "/usr/lib/libasound.so.2" {
		t.Fatalf("Midi name or firmware ALSA changed: %+v", p)
	}
	if err := validateControlRenderer(p, &controlsConfig{Voice: true, Ducking: true, NativeVolume: true, LEDs: true}); err != nil {
		t.Fatal(err)
	}
	conf := receiverControlConfig(p, &controlsConfig{Voice: true, NativeVolume: true})
	for _, setting := range []string{`output_device = "spk_tee"`, "output_rate = 96000", `output_format = "S16_LE"`, "output_channels = 2", "port = 5000", metadataFIFO, `ignore_volume_control = "yes"`} {
		if !strings.Contains(conf, setting) {
			t.Fatalf("missing Midi setting %q: %s", setting, conf)
		}
	}
	if strings.Contains(conf, `resync_threshold_in_seconds = 0;`) || strings.Contains(conf, "0.5;") {
		t.Fatal("direct PCM must report latency and keep normal synchronization")
	}
	if got := uiAsound(p.pcm, audioFIFO); !strings.Contains(got, `slave.pcm "speakers"`) || strings.Contains(got, "hw:0,0") || strings.Contains(got, "type null") {
		t.Fatalf("visualizer tee bypasses the shared mixer: %s", got)
	}
}

func TestMini3ClockRequiresCompleteCapabilities(t *testing.T) {
	features := []string{"display.clock.segment.v1", "display.clock.music.v1", "led.ring.rgb", "led.ring.6"}
	for _, model := range []string{"yandexmini3", "bergamot"} {
		for _, shared := range []bool{false, true} {
			p, ok := profileForDevice(yamDevice{Model: model, Features: features}, shared)
			if !ok || !p.ui || !p.sharedUI || p.leds != 6 || p.digitBase != 0 || strings.Join(p.colon, ",") != "DH,DL" {
				t.Fatalf("Mini3 missing shared clock geometry: %+v", p)
			}
			if p.pcm != "speakers" || p.receiverABI != "armhf" || !p.quasarCoexist {
				t.Fatal("changed Mini3 audio contract")
			}
			if err := validateControlRenderer(p, &controlsConfig{LEDs: true, NativeVolume: true}); err != nil {
				t.Fatal(err)
			}
			if cfg := receiverConfig(p); !strings.Contains(cfg, `output_device = "spk_tee"`) || !strings.Contains(cfg, `enabled = "yes"`) {
				t.Fatal("missing PCM/metadata tap")
			}
		}
		for missing := range features {
			partial := append([]string{}, features[:missing]...)
			partial = append(partial, features[missing+1:]...)
			p, ok := profileForDevice(yamDevice{Model: model, Features: partial}, false)
			if !ok || p.ui || p.sharedUI || p.pcm != "speakers" {
				t.Fatalf("unsafe Mini3 fallback: %+v", p)
			}
		}
	}
	p, _ := profileForDevice(yamDevice{Model: "yandexmini_3_pro", Features: features}, true)
	if p.ui || p.sharedUI {
		t.Fatal("Mini3 Pro enabled accidentally")
	}
}

func TestRuntimeEnvUsesFirmwareLoader(t *testing.T) {
	env := runtimeEnv(
		[]string{"PATH=/bin", "LD_LIBRARY_PATH=/old", "LD_PRELOAD=/old.so", "HOME=/"},
		"/pkg/lib", "/usr/lib/libasound.so.2",
	)
	got := map[string]string{}
	for _, s := range env {
		key, value, _ := strings.Cut(s, "=")
		got[key] = value
	}
	if got["PATH"] != "/bin" || got["HOME"] != "/" || len(env) != 4 {
		t.Fatalf("unexpected child env %v", env)
	}
	if got["LD_LIBRARY_PATH"] != "/pkg/lib" {
		t.Fatalf("library path %q", got["LD_LIBRARY_PATH"])
	}
	if got["LD_PRELOAD"] != "/usr/lib/libasound.so.2" {
		t.Fatalf("preload %q", got["LD_PRELOAD"])
	}
	if env := runtimeEnv([]string{"A=1"}, "/pkg/lib", ""); len(env) != 2 || env[1] != "LD_LIBRARY_PATH=/pkg/lib" {
		t.Fatalf("no-preload env %v", env)
	}
}
