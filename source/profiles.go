package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func watchParent(ctx context.Context, cancel context.CancelFunc, parent int, current func() int) {
	if parent <= 1 {
		return
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if current() != parent {
				cancel()
				return
			}
		}
	}
}

// High-volume receiver statistics stay in bounded RAM files, not YAM's flash log.
type boundedLog struct {
	mu sync.Mutex
	f  *os.File
	n  int
}

func (l *boundedLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	const limit = 128 * 1024
	length := len(b)
	if len(b) > limit {
		b = b[len(b)-limit:]
	}
	if l.n+len(b) > limit {
		if err := l.f.Truncate(0); err != nil {
			return 0, err
		}
		if _, err := l.f.Seek(0, 0); err != nil {
			return 0, err
		}
		l.n = 0
	}
	n, err := l.f.Write(b)
	l.n += n
	if err != nil {
		return n, err
	}
	return length, nil
}

func run(ctx context.Context, sharedAudio bool) error {
	return runWithControls(ctx, sharedAudio, nil, "")
}

// kernelArch reports the machine the kernel runs, not the userspace ABI this
// binary was built for. Station Street and Light 2 run ARMhf userspace on an
// AArch64 kernel, so the shared-audio contract must follow uname.
func kernelArch() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	b := make([]byte, 0, len(u.Machine))
	for _, c := range u.Machine {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

func runWithControls(ctx context.Context, sharedAudio bool, controls *controlsConfig, controlsPath string) error {
	api, err := newYAMAPI()
	if err != nil {
		return err
	}
	var d yamDevice
	err = waitForReady(ctx, "YAM device API", 32*time.Second, func() error {
		var e error
		d, e = api.device()
		return e
	})
	if err != nil {
		return err
	}
	// Existing installs keep their JSON across upgrades. Update the defaults
	// once per release, but never overwrite a value the owner changed.
	controls, err = migrateControls(controlsPath, d.Model, controls)
	if err != nil {
		fmt.Fprintf(os.Stderr, "station-airplay: controls defaults: %v\n", err)
	}
	p, ok := profileForDevice(d, sharedAudio)
	if !ok {
		return fmt.Errorf("unsupported YAM model %q", d.Model)
	}
	p = profileWithTiming(p, controls)
	if controls != nil && controls.Equalizer != nil && controls.Equalizer.Enabled {
		p.eqConfigPath = controls.configPath
	}
	fmt.Fprintf(os.Stderr, "station-airplay: timing mode=%s offset=%.1fs (receiver restart required to change)\n",
		p.timingMode, p.latencyOffset())
	// A model whose control profile is not qualified still has to play audio:
	// Mini 3 Pro has no qualified shared-control transport, and refusing to
	// start left it with a dead plugin. Station 3 shares the Mini gate.
	if err = validateControlRenderer(p, controls); err != nil {
		fmt.Fprintf(os.Stderr, "station-airplay: shared controls disabled: %v\n", err)
		controls = nil
	}
	p, err = profileWithRoom(p, d.Room)
	if err != nil {
		return err
	}
	if controls != nil && orionDevice(d) {
		if controls.LEDs {
			controls.LEDs = false
			fmt.Fprintln(os.Stderr, "shared controls: Station 3 ring left to Alice; session LED fill is not implemented")
		}
		if controls.NativePlayer {
			controls.NativePlayer = false
			fmt.Fprintln(os.Stderr, "shared controls: Station 3 skips native-player pause; it painted Alice's pause glyph after continue/next/back")
		}
	}
	if controls != nil {
		if err = controlsPreflight(ctx, api, d, controls); err != nil {
			if !p.sharedControls || !p.packagedDBus {
				return err
			}
			// The Mini must keep playing even when Luna cannot expose a control
			// capability (an emulator, an older host, a first boot). The packaged
			// bridge is an enhancement here, not a startup condition.
			fmt.Fprintf(os.Stderr, "station-airplay: Mini controls unavailable, continuing audio-only: %v\n", err)
			controls = nil
		}
	}
	if kernelArch() != "aarch64" {
		return errors.New("shared profiles require an AArch64 kernel")
	}
	return runSharedControls(ctx, p, api, d, controls)
}

// A renderer must have only one UI owner. Controls keep their explicit
// Light 2 audio-only profile; upstream display mode remains available without them.
func isLunaHost(api *yamAPI) bool {
	if api == nil {
		return false
	}
	_, err := lunaServerRoot(api.base)
	return err == nil
}

func validateControlRenderer(p stationProfile, controls *controlsConfig) error {
	if controls == nil {
		return nil
	}
	if p.ui && !isMidiProfile(p) && !p.sharedUI {
		return errors.New("shared controls on clock-equipped Light 2 require --shared-audio; upstream display mode cannot own the same UI")
	}
	if p.receiverABI == "aarch64" {
		return errors.New("shared controls require the control-enabled ARMhf receiver; AArch64 control build is not qualified")
	}
	if p.packagedDBus && !p.sharedControls {
		return errors.New("shared controls for the packaged D-Bus profile are not qualified; audio-only upstream mode remains available")
	}
	return nil
}

// Clock capability alone does not say whether shared audio is wanted or usable.
// Explicit opt-in also supports Plum firmware advertising a segment clock.
// Without opt-in, retain upstream 1.4.2 clock-equipped Light 2 display mode.
func profileForDevice(d yamDevice, sharedAudio bool) (stationProfile, bool) {
	p, ok := profileFor(d.Model)
	if ok && (d.Model == "plum" || d.Model == "yandexplum") && (sharedAudio || !feature(d, "display.clock.segment.v1")) {
		p = plumSharedProfile
		// Enable the known three-zone layout only with all its capabilities.
		// Clockless units and older hosts retain shared audio and controls.
		if feature(d, "display.clock.segment.v1") && feature(d, "display.clock.music.v1") &&
			feature(d, "led.ring.rgb") && feature(d, "led.ring.3") {
			p.ui, p.sharedUI = true, true
			p.leds, p.digitBase, p.colon = 3, 0, []string{"DH", "DL"}
		}
		return p, true
	}
	// Mini 3 previously used session RGB only. Require the new cooperative
	// backend as well as the exact six-zone geometry before taking the clock.
	if ok && (d.Model == "yandexmini3" || d.Model == "bergamot") &&
		feature(d, "display.clock.segment.v1") && feature(d, "display.clock.music.v1") &&
		feature(d, "led.ring.rgb") && feature(d, "led.ring.6") {
		p.ui, p.sharedUI = true, true
		p.leds, p.digitBase, p.colon = 6, 0, []string{"DH", "DL"}
	}
	// MIDI keeps its existing 1..4 digit geometry, 96 kHz shared PCM and
	// 24-LED ring. Only switch display ownership when Luna exposes both the
	// segment and clock-music APIs; older hosts retain the legacy renderer.
	if ok && d.Model == "yandexmidi" &&
		feature(d, "display.clock.segment.v1") && feature(d, "display.clock.music.v1") {
		p.ui, p.sharedUI = true, true
	}
	return p, ok
}

func isMidiProfile(p stationProfile) bool {
	return len(p.models) == 1 && p.models[0] == "yandexmidi"
}

func profileOutputRate(p stationProfile) int {
	if p.outputRate != 0 {
		return p.outputRate
	}
	return 48000
}

func profileRTSPPort(p stationProfile) int {
	if p.rtspPort != 0 {
		return p.rtspPort
	}
	return 5000
}

func orionDevice(d yamDevice) bool {
	return d.Model == "yandexstation_3" || d.Model == "orion"
}

func profileIsOrion(p stationProfile) bool {
	for _, m := range p.models {
		if m == "yandexstation_3" || m == "orion" {
			return true
		}
	}
	return false
}

func ifaceMAC(name string) string {
	b, err := os.ReadFile("/sys/class/net/" + name + "/address")
	if err != nil {
		return ""
	}
	hex := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(string(b)), ":", ""))
	if len(hex) != 12 {
		return ""
	}
	return hex
}

func ifaceIPv4(name string) string {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		return ip.String()
	}
	return ""
}

func parseDefaultRoute(text string) string {
	best, metric := "", int(^uint(0)>>1)
	for i, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if i == 0 || len(fields) < 8 || fields[1] != "00000000" {
			continue
		}
		name := fields[0]
		if name == "lo" || name == "p2p0" {
			continue
		}
		n, err := strconv.Atoi(fields[6])
		if err != nil {
			continue
		}
		if n < metric {
			best, metric = name, n
		}
	}
	return best
}

func defaultRouteInterface() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	return parseDefaultRoute(string(b))
}

func primaryLAN() (name, ipv4 string) {
	if iface := defaultRouteInterface(); iface != "" {
		if ip := ifaceIPv4(iface); ip != "" {
			return iface, ip
		}
		return iface, ""
	}
	for _, nic := range []string{"wlan0", "eth0"} {
		if ip := ifaceIPv4(nic); ip != "" {
			return nic, ip
		}
	}
	return "", ""
}

func orionBindConfig() string {
	extra := ` nqptp_shared_memory_interface_name = "/nqptp";
`
	name, ip := primaryLAN()
	if name != "" {
		extra += fmt.Sprintf(" interface = %q;\n", name)
	}
	if ip != "" {
		extra += fmt.Sprintf(" address = %q;\n", ip)
	}
	if name != "" {
		if hex := ifaceMAC(name); hex != "" {
			extra += fmt.Sprintf(" airplay_device_id = 0x%sL;\n", hex)
			return extra
		}
	}
	for _, nic := range []string{"wlan0", "eth0"} {
		if hex := ifaceMAC(nic); hex != "" {
			extra += fmt.Sprintf(" airplay_device_id = 0x%sL;\n", hex)
			break
		}
	}
	return extra
}

func receiverConfig(p stationProfile) string {
	device := p.pcm
	meta := `metadata = { enabled = "no"; };`
	if p.ui {
		// spk_tee copies PCM into the visualizer FIFO and plays the same
		// frames on the shared mixer. The mixer name stays in uiAsound.
		device = "spk_tee"
		meta = fmt.Sprintf("metadata = { enabled = \"yes\"; include_cover_art = \"no\"; pipe_name = %q; };", metadataFIFO)
	}
	deviceID := ""
	if profileIsOrion(p) {
		deviceID = orionBindConfig()
	}
	return fmt.Sprintf(`general = {
 name = %q;
 model = %q;
 output_backend = "alsa";
 mdns_backend = "avahi";
 port = %d;
 interpolation = "basic";
 volume_range_db = 50;
 resync_threshold_in_seconds = 0.050;
 drift_tolerance_in_seconds = 0.002;
 // Stay well inside AirPlay's own buffering: the stock 0.2 s backend buffer
 // adds avoidable start-up and resume delay in front of the shared mixer.
 audio_backend_buffer_desired_length_in_seconds = 0.10;
 // Synchronized mode preserves negotiated AirPlay presentation time.
 // fast_audio explicitly trades video/multiroom synchronization for a
 // shorter apparent delay; backend buffering is independent of this mode.
 audio_backend_latency_offset_in_seconds = %.1f;
%s};
sessioncontrol = { allow_session_interruption = "yes"; session_timeout = 60; };
alsa = {
 output_device = %q;
 output_rate = %d;
 output_channels = 2;
 output_format = "S16_LE";
 disable_standby_mode = "never";
};
%s
`, p.name, p.airplayModel, profileRTSPPort(p), p.latencyOffset(), deviceID, device, profileOutputRate(p), meta)
}

// uiAsound is the receiver's private ~/.asoundrc. The file plugin feeds the
// clock visualizer; its slave is the firmware shared mixer, not hw:0,0.
func uiAsound(pcm, fifo string) string {
	return uiAsoundSpectrum(pcm, fifo, "")
}

// uiAsoundSpectrum optionally adds a second file PCM in the chain so the
// analyser gets its own copy of the same PCM without a second FIFO reader.
func uiAsoundSpectrum(pcm, clockFIFO, spectrumFIFO string, rateConverter ...string) string {
	return uiAsoundSpectrumAtRate(pcm, clockFIFO, spectrumFIFO, 48000, rateConverter...)
}

func uiAsoundSpectrumAtRate(pcm, clockFIFO, spectrumFIFO string, rate int, rateConverter ...string) string {
	// Both file taps copy the forced S16_LE stereo stream. Conversion to the
	// hardware's native format happens below them, never before the analyser.
	outputBlock := fmt.Sprintf(`pcm.spk_output {
 type plug
 slave.pcm %q
}
`, pcm)
	stream := "spk_output"
	var spectrumBlock string
	if spectrumFIFO != "" {
		spectrumBlock = fmt.Sprintf(`pcm.spk_spectrum_file {
 type file
 slave.pcm %q
 file %q
 format "raw"
 perm 0666
}
`, stream, spectrumFIFO)
		stream = "spk_spectrum_file"
	}
	var clockBlock string
	if clockFIFO != "" {
		clockBlock = fmt.Sprintf(`pcm.spk_file {
 type file
 slave.pcm %q
 file %q
 format "raw"
 perm 0666
}
`, stream, clockFIFO)
		stream = "spk_file"
	}
	// Name the converter explicitly where the firmware's ALSA plugin
	// directory is missing. Yandex resamples to 48 kHz before ALSA, so its own
	// plug never needs this module; the receiver gets 44.1 kHz from AirPlay.
	converter := ""
	if len(rateConverter) > 0 && rateConverter[0] != "" {
		converter = fmt.Sprintf("defaults.pcm.rate_converter %q\n", rateConverter[0])
	}
	return converter + outputBlock + spectrumBlock + clockBlock + fmt.Sprintf(`pcm.spk_tee {
 type plug
 slave {
  pcm %q
  rate %d
  format "S16_LE"
  channels 2
 }
}
`, stream, rate)
}

func receiverControlConfig(p stationProfile, controls *controlsConfig, artwork ...bool) string {
	p = profileWithTiming(p, controls)
	config := receiverConfig(p)
	if controls == nil {
		return config
	}
	withArtwork := len(artwork) > 0 && artwork[0]
	// Only the PCM analyser needs the tee. A solid artwork frame reads the
	// receiver's cover cache and leaves the audio path alone.
	withSpectrum := withArtwork && controls.useSpectrum()
	if withSpectrum {
		// The ALSA file plugin copies the same PCM into the analyser FIFO and
		// passes it through to the station mixer. Audio is not re-routed.
		config = strings.Replace(config, `output_device = "`+p.pcm+`"`, `output_device = "spk_tee"`, 1)
	}
	// Shairport's internal metadata hub also carries DACP credentials/client state.
	// Disabling metadata disables remote-control discovery even with D-Bus built.
	// Keep its unused FIFO private and volatile; never request artwork/cache files.
	config = strings.Replace(config, `metadata = { enabled = "no"; };`,
		fmt.Sprintf(`metadata = { enabled = "yes"; include_cover_art = "no"; pipe_name = %q; cover_art_cache_directory = %q; };`, controlMetadataFIFO, coverArtDir), 1)
	if withArtwork {
		// Normalise the cache key first so the non-UI template does not end up
		// with a duplicate.
		config = strings.Replace(config, `; cover_art_cache_directory = "/run/station-airplay/coverart"`, "", 1)
		config = strings.Replace(config, `include_cover_art = "no"`,
			`include_cover_art = "yes"; cover_art_cache_directory = "/run/station-airplay/coverart"`, 1)
	}
	// The default ten-second active tail keeps the playback indication lit after
	// AirPlay has gone. A short tail still absorbs jitter without lingering.
	config = strings.Replace(config, `sessioncontrol = { allow_session_interruption = "yes"; session_timeout = 60; };`,
		`sessioncontrol = { allow_session_interruption = "yes"; session_timeout = 60; active_state_timeout = 1.0; };`, 1)
	if controls.NativeVolume {
		config = strings.Replace(config, " volume_range_db = 50;", " volume_range_db = 50;\n ignore_volume_control = \"yes\";", 1)
	}
	return config
}

func audioEnv(environ []string, home, bus string) []string {
	env := []string{}
	for _, s := range environ {
		if strings.HasPrefix(s, "YAM_API_") || strings.HasPrefix(s, "LUNA_API_") || strings.HasPrefix(s, "LD_LIBRARY_PATH=") || (bus != "" && strings.HasPrefix(s, "DBUS_SYSTEM_BUS_ADDRESS=")) || strings.HasPrefix(s, "HOME=") || strings.HasPrefix(s, "ALSA_CONFIG_PATH=") {
			continue
		}
		env = append(env, s)
	}
	if bus != "" {
		env = append(env, "DBUS_SYSTEM_BUS_ADDRESS=unix:path="+bus)
	}
	return append(env, "HOME="+home)
}

// Stock mDNS already owns this address/name. Publishing another address record
// triggers a rename loop on these firmwares, even with a different host label.
func stockHostname(prefix, cmdline string) (string, error) {
	serial := ""
	for _, field := range strings.Fields(cmdline) {
		if strings.HasPrefix(field, "androidboot.serialno=") {
			serial = strings.TrimPrefix(field, "androidboot.serialno=")
		}
	}
	return joinHostID(prefix, serial)
}

// Linkplay factory settings (JSON). The Mini's maind publishes its mv_uuid.
const linkplayNvram = "/vendor/new_nvram_config"

func nvramHostname(prefix string, raw []byte) (string, error) {
	var nv struct {
		UUID string `json:"mv_uuid"`
	}
	if err := json.Unmarshal(raw, &nv); err != nil {
		return "", fmt.Errorf("Linkplay nvram: %w", err)
	}
	return joinHostID(prefix, nv.UUID)
}

func joinHostID(prefix, id string) (string, error) {
	if id == "" || len(id) > 32 {
		return "", errors.New("missing/invalid device id for stock mDNS hostname")
	}
	for _, c := range id {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return "", errors.New("invalid device id")
		}
	}
	return prefix + "-" + id, nil
}

func stockHost(p stationProfile) (string, error) {
	if p.nvramHostID {
		b, e := os.ReadFile(linkplayNvram)
		if e == nil {
			return nvramHostname(p.mdnsHostPrefix, b)
		}
		if !errors.Is(e, os.ErrNotExist) {
			return "", e
		}
		// Bench images and repaired boards may not carry the Linkplay factory
		// file. A missing file must not take AirPlay down: fall back to the same
		// serial-based identity every other model uses.
		fmt.Fprintf(os.Stderr, "station-airplay: %s missing, using serial host id\n", linkplayNvram)
	}
	b, e := os.ReadFile("/proc/cmdline")
	if e != nil {
		return "", e
	}
	return stockHostname(p.mdnsHostPrefix, string(b))
}

// prepareALSAConfig writes the receiver's private ALSA configuration. The
// firmware's alsa.conf includes ~/.asoundrc through an optional hook; the
// Mini's vendor alsa-lib may not, and shairport silences ALSA diagnostics, so
// a silently ignored file looks like ENOENT. Pointing ALSA_CONFIG_PATH at a
// file that includes the system configuration and the analyser chain removes
// that ambiguity. Returns "" when the firmware has no readable system config.
func prepareALSAConfig(state, asoundrc string) (string, error) {
	body, err := os.ReadFile(asoundrc)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	system := ""
	for _, name := range []string{"/usr/share/alsa/alsa.conf", "/etc/alsa/alsa.conf"} {
		if info, e := os.Stat(name); e == nil && info.Mode().IsRegular() {
			system = name
			break
		}
	}
	if system == "" {
		return "", nil
	}
	config := filepath.Join(state, "alsa.conf")
	var b strings.Builder
	fmt.Fprintf(&b, "</%s>\n", strings.TrimPrefix(system, "/"))
	if info, e := os.Stat("/etc/asound.conf"); e == nil && info.Mode().IsRegular() {
		b.WriteString("</etc/asound.conf>\n")
	}
	b.Write(body)
	if err = os.WriteFile(config, []byte(b.String()), 0600); err != nil {
		return "", err
	}
	return config, nil
}

// dbusCommand returns the D-Bus tool: firmware /usr/bin or the packaged ARMhf
// build. Packaged children receive LD_LIBRARY_PATH from runtimeEnv.
func dbusCommand(root string, p stationProfile, tool string) (string, []string) {
	if !p.packagedDBus {
		return "/usr/bin/" + tool, nil
	}
	return filepath.Join(root, "bin", tool), nil
}

// legacyRuntimeLoader reports the bundled armhf loader when a package carries
// its own libc. Only the original Mini firmware is old enough to need it: its
// glibc is 2.23 while the rebuilt runtime needs 2.25..2.29. Packages for newer
// firmware do not contain the loader and keep using the firmware libc.
func legacyRuntimeLoader(root string) string {
	path := filepath.Join(root, "lib", "ld-linux-armhf.so.3")
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path
	}
	return ""
}

// withLegacyRuntime runs a packaged binary through the bundled loader so the
// package libc, not the firmware one, resolves its symbols. Firmware tools and
// packages without the loader are returned unchanged.
// Additional firmware directories follow the package directory, preserving the
// private libc while allowing firmware ALSA plugins to resolve their helpers.
func withLegacyRuntime(root, bin string, args []string, extraLibs ...string) (string, []string) {
	loader := legacyRuntimeLoader(root)
	prefix := filepath.Join(root, "bin") + string(filepath.Separator)
	if loader == "" || !strings.HasPrefix(bin, prefix) {
		return bin, args
	}
	libs := append([]string{filepath.Join(root, "lib")}, extraLibs...)
	return loader, append([]string{"--library-path", strings.Join(libs, string(os.PathListSeparator)), bin}, args...)
}

func firmwareALSA(p stationProfile) string {
	if p.alsaLib != "" {
		return p.alsaLib
	}
	return "/usr/lib/libasound.so.2"
}

type childFailure struct {
	name string
	err  error
}

func (e *childFailure) Error() string {
	return fmt.Sprintf("%s exited unexpectedly: %v", e.name, e.err)
}

func runShared(ctx context.Context, p stationProfile) error {
	return runSharedControls(ctx, p, nil, yamDevice{}, nil)
}
func runSharedControls(ctx context.Context, p stationProfile, api *yamAPI, d yamDevice, controls *controlsConfig) error {
	for attempts := 0; ; attempts++ {
		err := serveSharedControls(ctx, p, api, d, controls)
		if ctx.Err() != nil {
			return nil
		}
		var failed *childFailure
		if !errors.As(err, &failed) || attempts >= 5 {
			return err
		}
		fmt.Fprintf(os.Stderr, "station-airplay: %v; restarting owned children in 3 seconds (attempt %d/5)\n", err, attempts+1)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

func serveShared(ctx context.Context, p stationProfile) error {
	return serveSharedControls(ctx, p, nil, yamDevice{}, nil)
}
func serveSharedControls(ctx context.Context, p stationProfile, api *yamAPI, d yamDevice, controls *controlsConfig) error {
	if err := validateControlRenderer(p, controls); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(exe))
	state := "/run/station-airplay"
	if err = os.MkdirAll(state, 0700); err != nil {
		return err
	}
	lk, err := os.OpenFile(filepath.Join(state, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lk.Close()
	if err = waitForReady(ctx, "previous station-airplay retirement", 3*time.Second, func() error {
		return syscall.Flock(int(lk.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}); err != nil {
		return err
	}
	defer syscall.Flock(int(lk.Fd()), syscall.LOCK_UN)
	if err = preflight(p); err != nil {
		return err
	}
	// Experimental processing is optional: an unavailable firmware player must
	// not prevent the receiver (or its control panel) from starting.
	p, eqBlock, eqWarning := prepareOptionalEqualizer(p, exe, exec.LookPath)
	if eqWarning != "" {
		fmt.Fprintln(os.Stderr, "station-airplay:", eqWarning)
	}
	if eqBlock != "" {
		if err = os.WriteFile(filepath.Join(state, ".asoundrc"), nil, 0600); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "station-airplay: experimental AirPlay-only EQ enabled; pipe timing and CPU require live validation")
	}
	var metaFD, audioFD int
	// -1 means "no analyser FIFO": a solid artwork frame reads only the
	// receiver's cover cache.
	spectrumFD := -1
	var pcm <-chan visualizerPCM
	var pcmErrors <-chan error
	var tap *spectrumTap
	// Artwork drives both the PCM spectrum and the solid indication colour, so
	// the receiver asks for covers whenever the LEDs are on. Only the PCM tee
	// and the analyser depend on the spectrum option.
	artwork := controls != nil && controls.LEDs && spectrumSupported(d)
	spectrum := artwork && controls.useSpectrum()
	if p.ui {
		if p.sharedUI {
			// The selected profile has already checked its capabilities. Do
			// not paint a startup probe over Alice or the firmware clock.
			hw = p
		} else {
			if api, err = newYAMAPI(); err != nil {
				return err
			}
			if err = verifyYAM(api); err != nil {
				return err
			}
		}
		if spectrum {
			if err = os.MkdirAll(coverArtDir, 0700); err != nil {
				return err
			}
			if spectrumFD, err = ensureFIFO(spectrumFIFO); err != nil {
				return err
			}
			defer syscall.Close(spectrumFD)
		}
		clockFIFO, spectrumTapFIFO := audioFIFO, ""
		if spectrum {
			spectrumTapFIFO = spectrumFIFO
		}
		if err = os.WriteFile(filepath.Join(state, ".asoundrc"), []byte(uiAsoundSpectrumAtRate(p.pcm, clockFIFO, spectrumTapFIFO, profileOutputRate(p), p.alsaRateConverter)), 0600); err != nil {
			return err
		}
		if audioFD, err = ensureFIFO(audioFIFO); err != nil {
			return err
		}
		defer syscall.Close(audioFD)
		if metaFD, err = ensureFIFO(metadataFIFO); err != nil {
			return err
		}
		defer syscall.Close(metaFD)
		if spectrum {
			tap = newSpectrumTap(coverArtDir)
			tap.state.sampleRate = profileOutputRate(p)
			if err = configureSpectrum(d, *controls, tap); err != nil {
				return err
			}
			go tap.run(ctx, spectrumFD)
		}
		var stopPCM func()
		pcm, pcmErrors, stopPCM = startVisualizerPCM(audioFD)
		defer stopPCM() // Registered before child cleanup: keep draining until they stop.
	} else if artwork {
		if err = os.MkdirAll(coverArtDir, 0700); err != nil {
			return err
		}
		if spectrum {
			if err = os.WriteFile(filepath.Join(state, ".asoundrc"), []byte(uiAsoundSpectrumAtRate(p.pcm, "", spectrumFIFO, profileOutputRate(p), p.alsaRateConverter)), 0600); err != nil {
				return err
			}
			if spectrumFD, err = ensureFIFO(spectrumFIFO); err != nil {
				return err
			}
			defer syscall.Close(spectrumFD)
		}
		// The receiver still publishes metadata and artwork here; drain its
		// FIFO so a full buffer cannot block playback while the display
		// renderer is not running.
		if metaFD, err = ensureFIFO(controlMetadataFIFO); err != nil {
			return err
		}
		defer syscall.Close(metaFD)
		go drainFIFO(ctx, metaFD)
		tap = newSpectrumTap(coverArtDir)
		tap.state.sampleRate = profileOutputRate(p)
		if !spectrum {
			// A solid frame still needs the per-model gains and neutral-black
			// rule; the spectrum path configures the tap when it starts.
			if err = configureSpectrum(d, *controls, tap); err != nil {
				return err
			}
		}
		go tap.run(ctx, spectrumFD)
	}
	if eqBlock != "" {
		file, e := os.OpenFile(filepath.Join(state, ".asoundrc"), os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = file.WriteString(eqBlock)
		closeErr := file.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, port := range []int{319, 320, 9000} {
		socket, e := net.ListenPacket("udp4", fmt.Sprintf(":%d", port))
		if e != nil {
			return fmt.Errorf("AirPlay 2 timing port UDP %d occupied: %w", port, e)
		}
		socket.Close()
	}
	alsaConfig, err := prepareALSAConfig(state, filepath.Join(state, ".asoundrc"))
	if err != nil {
		return err
	}
	if alsaConfig != "" {
		fmt.Fprintln(os.Stderr, "station-airplay: private ALSA config", alsaConfig)
	}
	conf := filepath.Join(state, "shairport.conf")
	config := receiverControlConfig(p, controls, tap != nil)
	if err = os.WriteFile(conf, []byte(config), 0600); err != nil {
		return err
	}
	bus, err := preparePrivateMDNS(state, "/etc")
	if err != nil {
		return err
	}
	env := audioEnv(os.Environ(), state, bus)
	libs := filepath.Join(root, "lib")
	libEnv := runtimeEnv(env, libs, "")
	children := []*child{}
	controlDone := make(chan struct{})
	controlErrors := make(chan error, 1)
	controlsStarted := false
	controlUpdates := make(chan rendererControlState, 1)
	defer func() {
		cancel()
		if controlsStarted {
			<-controlDone
		}
		stopChildren(children)
	}()
	exits := make(chan exitInfo, 4)
	launchWith := func(name, bin string, childEnv []string, args ...string) error {
		file, e := os.OpenFile(filepath.Join(state, name+".log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		c, e := startChild(ctx, bin, args, childEnv, &boundedLog{f: file})
		if e != nil {
			file.Close()
			return e
		}
		c.name = name
		fmt.Printf("started %s pid=%d\n", name, c.cmd.Process.Pid)
		children = append(children, c)
		go func() {
			err := c.cmd.Wait()
			file.Close()
			exits <- exitInfo{c.name, err}
		}()
		return nil
	}
	childEnv := func(bin string) []string {
		if strings.HasPrefix(bin, root+string(filepath.Separator)) {
			return libEnv
		}
		// Firmware tools such as /usr/bin/dbus-daemon must keep their own libdbus.
		return env
	}
	launch := func(name, bin string, args ...string) error {
		bin, args = withLegacyRuntime(root, bin, args)
		return launchWith(name, bin, childEnv(bin), args...)
	}
	if err = startPrivateMDNS(ctx, root, state, p, childEnv, launch, exits); err != nil {
		return err
	}
	ready := func(stage string, check func(context.Context) error) error {
		return waitSharedStartup(ctx, stage, sharedStartupTimeout, sharedStartupInterval, exits, check)
	}
	launchAP2 := func(name string, args ...string) error {
		binary, childEnv := name, libEnv
		var firmwareLibs []string
		if name == "shairport-sync" {
			binary = "shairport-sync-ap2"
			if p.receiverABI == "aarch64" {
				binary += "-aarch64"
				childEnv = runtimeEnv(env, filepath.Join(libs, "aarch64")+":/usr/lib:/lib", "")
			} else {
				// Shared dmix must use the firmware ALSA ABI, not the legacy Light library.
				if p.alsaLib != "" {
					firmwareLibs = append(firmwareLibs, filepath.Dir(p.alsaLib))
				}
				libraryPath := strings.Join(append([]string{libs}, firmwareLibs...), string(os.PathListSeparator))
				childEnv = runtimeEnv(env, libraryPath, firmwareALSA(p))
				if alsaConfig != "" {
					childEnv = append(childEnv, "ALSA_CONFIG_PATH="+alsaConfig)
				}
			}
		}
		path, args := withLegacyRuntime(root, filepath.Join(root, "bin", binary), args, firmwareLibs...)
		return launchWith(name, path, childEnv, args...)
	}
	if err = launchAP2("nqptp", "-v"); err != nil {
		return err
	}
	if err = ready("nqptp UDP 9000", func(context.Context) error {
		socket, e := net.ListenPacket("udp4", "127.0.0.1:9000")
		if e == nil {
			socket.Close()
			return errors.New("nqptp has not bound UDP 9000")
		}
		return nil
	}); err != nil {
		return err
	}
	if err = launchAP2("shairport-sync", "-c", conf, "-v", "--statistics"); err != nil {
		return err
	}
	if controls != nil {
		if err = ready("receiver controls D-Bus API", func(parent context.Context) error {
			r, e := openReceiver(bus)
			if e != nil {
				return fmt.Errorf("open receiver: %w", e)
			}
			defer r.conn.Close()
			check, done := context.WithTimeout(parent, 3*time.Second)
			defer done()
			_, e = r.State(check)
			if e == nil && controls.Ducking {
				e = r.Gain(check, 100, 0)
			}
			return e
		}); err != nil {
			return fmt.Errorf("shared controls need receiver D-Bus support: %w", err)
		}
		controlsStarted = true
		go func() {
			defer close(controlDone)
			var observer func(rendererControlState)
			if p.ui {
				observer = func(update rendererControlState) {
					select {
					case <-controlUpdates:
					default:
					}
					select {
					case controlUpdates <- update:
					default:
					}
				}
			}
			if e := serveControlsUI(ctx, bus, api, d, *controls, observer, tap, tap != nil && !p.ui); e != nil {
				controlErrors <- e
			}
		}()
	}
	if p.ui {
		var spectrumLevel uint8
		if controls != nil {
			spectrumLevel = spectrumLEDLevel(*controls)
		}
		fmt.Printf("%s model=%s protocol=AirPlay2 pcm=%s rate=%d Alice=shared UI=clock+led\n", version, p.models[0], p.pcm, profileOutputRate(p))
		return renderDisplay(ctx, api, metaFD, exits, displayInput{profile: p, controls: controls, pcm: pcm, pcmErrors: pcmErrors, updates: controlUpdates, controlErrors: controlErrors, tap: tap, spectrumLevel: spectrumLevel})
	}
	fmt.Printf("%s model=%s protocol=AirPlay2 pcm=%s Alice=shared UI=off\n", version, p.models[0], p.pcm)
	select {
	case <-ctx.Done():
		return nil
	case e := <-controlErrors:
		return fmt.Errorf("shared controls stopped: %w", e)
	case ex := <-exits:
		return &childFailure{ex.name, ex.err}
	}
}

// drainFIFO keeps a metadata pipe moving when no renderer consumes it. The
// artwork itself is read from the receiver's cache, not from this stream.
func drainFIFO(ctx context.Context, fd int) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_ = readFD(fd, func([]byte) {})
		}
	}
}
