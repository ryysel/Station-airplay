package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPreviewYieldsWithoutBlankingAlice(t *testing.T) {
	c, _, s := testControls()
	c.preview = &ledPreview{}
	now := time.Now()
	_, err := c.preview.acquire("", []string{"#112233", "#112233", "#112233"}, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	c.preview.gate(now, true, "")
	c.preview.paint(context.Background(), c, now)
	if len(s.frames) != 1 {
		t.Fatal("preview not drawn")
	}
	c.preview.gate(now, false, "Alice active")
	c.preview.paint(context.Background(), c, now)
	if s.off != 0 || len(s.frames) != 1 {
		t.Fatal("preview blanked or painted over Alice")
	}
}

func TestControlPanelUnixRoutes(t *testing.T) {
	path, cfg := writeControlsFile(t, `{"timing_mode":"synchronized"}`)
	c, _, _ := testControls()
	c.preview = &ledPreview{}
	c.cfg = *cfg
	socket := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, err := startControlPanel(ctx, c, path, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	response, err := client.Get("http://localhost/settings")
	if err != nil {
		t.Fatal(err)
	}
	var read controlsConfig
	if err = json.NewDecoder(response.Body).Decode(&read); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || read.TimingMode != timingSynchronized {
		t.Fatal(response.Status, read)
	}
	// Two panels read the same revision. A stale full snapshot must not undo the first save.
	response, err = client.Get("http://localhost/settings-versioned")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct{ Revision string }
	err = json.NewDecoder(response.Body).Decode(&snapshot)
	response.Body.Close()
	if err != nil || snapshot.Revision == "" {
		t.Fatal(snapshot, err)
	}
	for index, want := range []int{200, 409} {
		timing := []string{"fast_audio", "synchronized"}[index]
		payload := `{"revision":"` + snapshot.Revision + `","settings":{"timing_mode":"` + timing + `"}}`
		req, _ := http.NewRequest("PUT", "http://localhost/settings-versioned", strings.NewReader(payload))
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("save %d: got %d want %d", index, res.StatusCode, want)
		}
	}
	current, err := loadControls(path)
	if err != nil || current.TimingMode != timingFastAudio {
		t.Fatal(current, err)
	}
	request, _ := http.NewRequest("POST", "http://localhost/preview", strings.NewReader(`{"colors":["#123456","#123456","#123456"]}`))
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
	info, _ := os.Stat(socket)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}

func TestPreviewOwnershipExpiryAndGate(t *testing.T) {
	var p ledPreview
	now := time.Unix(1000, 0)
	id, err := p.acquire("", []string{"#123456"}, 1, now)
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	if _, err = p.acquire("", []string{"#FFFFFF"}, 1, now); err == nil {
		t.Fatal("stole lease")
	}
	if err = p.stop("wrong"); err == nil {
		t.Fatal("stopped another session")
	}
	p.gate(now, true, "")
	if !p.status(now)["allowed"].(bool) {
		t.Fatal("safe gate blocked")
	}
	if p.status(now.Add(2 * time.Second))["allowed"].(bool) {
		t.Fatal("stale gate allowed")
	}
	p.gate(now, false, "Alice active")
	if p.status(now)["allowed"].(bool) {
		t.Fatal("Alice overwritten")
	}
	if p.active(now.Add(previewTTL)) {
		t.Fatal("lease did not expire")
	}
	if _, err = p.acquire("", []string{"#000000"}, 1, now.Add(previewTTL)); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewRejectsInvalidFrames(t *testing.T) {
	var p ledPreview
	for _, colors := range [][]string{nil, {"red"}, {"#000000", "#FFFFFF"}} {
		if _, err := p.acquire("", colors, 1, time.Now()); err == nil {
			t.Fatal(colors)
		}
	}
}

func TestPreviewIdleVolumeOverlayHasTwoSecondPriority(t *testing.T) {
	var p ledPreview
	now := time.Unix(1000, 0)
	p.acquire("", []string{"#FFFFFF"}, 1, now)
	p.observeVolume(20, now)
	p.gate(now, true, "")
	if !p.status(now)["allowed"].(bool) {
		t.Fatal("initial level blocked preview")
	}
	p.observeVolume(25, now.Add(time.Second))
	p.gate(now.Add(time.Second), true, "")
	if p.status(now.Add(time.Second))["allowed"].(bool) {
		t.Fatal("preview overwrote volume")
	}
	p.gate(now.Add(3*time.Second), true, "")
	if !p.status(now.Add(3 * time.Second))["allowed"].(bool) {
		t.Fatal("overlay did not expire")
	}
}

func TestPanelSettingsAtomicValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controls.json")
	original := []byte(`{"voice":true,"timing_mode":"fast_audio"}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"timing_mode":"bad"}`, `{"unknown":true}`, `{} {}`} {
		if err := savePanelSettings(path, []byte(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
		got, _ := os.ReadFile(path)
		if string(got) != string(original) {
			t.Fatal("invalid save changed file")
		}
	}
	if err := savePanelSettings(path, []byte(`{"voice":true,"timing_mode":"synchronized"}`)); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadControls(path)
	if err != nil || cfg.TimingMode != timingSynchronized || !cfg.Voice {
		t.Fatal(cfg, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("permissions", info.Mode())
	}
	backup, err := os.ReadFile(path + ".panel-backup")
	if err != nil || string(backup) != string(original) {
		t.Fatal("missing exact backup", err)
	}
}

func TestCalibrationConfigValidation(t *testing.T) {
	for _, data := range []string{`{"spectrum_black_floor":0.31}`, `{"spectrum_black_color":"purple"}`, `{"spectrum_saturation":0.5}`} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadControls(path); err == nil {
			t.Fatal("invalid tuning accepted", data)
		}
	}
}
