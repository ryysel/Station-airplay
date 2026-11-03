package main

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func speechBytes(field uint64, body []byte) []byte {
	out := binary.AppendUvarint(nil, field<<3|2)
	out = binary.AppendUvarint(out, uint64(len(body)))
	return append(out, body...)
}
func speechInt(field, value uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, field<<3), value)
}
func speechFixture(state uint64, request, text string, final, envelope bool) []byte {
	b := speechInt(1, state)
	b = append(b, speechBytes(7, []byte(request))...)
	r := speechBytes(1, []byte(text))
	if final {
		r = append(r, speechInt(2, 1)...)
	}
	b = append(b, speechBytes(15, r)...)
	b = append(b, speechBytes(14, []byte("private reply must not be exposed"))...)
	if envelope {
		return append(speechBytes(252, b), speechInt(253, 3)...)
	}
	return speechBytes(3, b)
}

func TestPauseOnlyExactPhrases(t *testing.T) {
	for _, phrase := range []string{"пауза", "стоп", "Алиса, пауза!", "АЛИСА, СТОП."} {
		if !pauseOnlyPhrase(phrase) {
			t.Fatal(phrase)
		}
	}
	for _, phrase := range []string{"не ставь на паузу", "что такое стоп", "стоп таймер", "расскажи про паузу", "продолжи", "включи музыку", "пауза и включи свет", ""} {
		if pauseOnlyPhrase(phrase) {
			t.Fatalf("unrelated phrase claimed: %q", phrase)
		}
	}
}
func TestDecodeNativePauseEnvelopeAndPrivacy(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		e, ok, err := decodeNativePause(speechFixture(3, "request-1", "Алиса, пауза", true, envelope))
		if err != nil || !ok || e.phase != "busy" || !e.final || !e.matched || e.request != "request-1" {
			t.Fatalf("%+v %v %v", e, ok, err)
		}
	}
	e, _, err := decodeNativePause(speechFixture(1, "request-1", "пауза", false, true))
	if err != nil || e.final {
		t.Fatal("partial recognition promoted to final", err)
	}
	legacy := append(speechInt(1, 3), speechBytes(2, []byte("стоп"))...)
	legacy = append(legacy, speechBytes(7, []byte("legacy-id"))...)
	e, ok, err := decodeNativePause(speechBytes(3, legacy))
	if err != nil || !ok || !e.final || !e.matched {
		t.Fatal("legacy BUSY recognition rejected", e, err)
	}
	for _, raw := range [][]byte{{255}, {10, 255}, {0}, {11}} {
		if _, _, err := decodeNativePause(raw); err == nil {
			t.Fatalf("malformed frame accepted: %x", raw)
		}
	}
}
func TestDecodeNativeCommandSpotter(t *testing.T) {
	command := speechBytes(3, []byte("стоп"))
	body := append(speechBytes(7, []byte("CommandSpotterCapability")), speechBytes(15, command)...)
	e, ok, err := decodeNativePause(append(speechBytes(252, body), speechInt(253, 210)...))
	if err != nil || !ok || !e.local || !e.matched || !e.final {
		t.Fatal(e, ok, err)
	}
}

func TestNativeVoicePauseFreshFinalAndDuplicate(t *testing.T) {
	_, r, s := testControls()
	p := pauseVoiceController{receiver: r}
	now := time.Now()
	ctx := context.Background()
	if ok, err := p.handle(ctx, nativePauseEvent{phase: "listening", request: "request-1", at: now}, now); ok || err != nil {
		t.Fatal(ok, err)
	}
	e := nativePauseEvent{phase: "listening", request: "request-1", matched: true, at: now}
	if ok, _ := p.handle(ctx, e, now); ok {
		t.Fatal("partial command sent")
	}
	e.phase, e.final = "busy", true
	if ok, err := p.handle(ctx, e, now); !ok || err != nil {
		t.Fatal("final pause rejected", ok, err)
	}
	if ok, _ := p.handle(ctx, e, now.Add(time.Second)); ok {
		t.Fatal("duplicate command sent")
	}
	if len(r.commands) != 1 || r.commands[0] != "Pause" || len(s.sets) != 0 {
		t.Fatal("unexpected transport or volume write", r.commands, s.sets)
	}
}
func TestNativeVoicePauseRejectsStaleSnapshotAndSenderChange(t *testing.T) {
	for _, kind := range []string{"snapshot", "stale", "no-sender", "changed-sender", "no-request", "idle"} {
		t.Run(kind, func(t *testing.T) {
			_, r, _ := testControls()
			p := pauseVoiceController{receiver: r}
			now := time.Now()
			ctx := context.Background()
			if kind == "no-sender" {
				r.s.Client = ""
			}
			_, _ = p.handle(ctx, nativePauseEvent{phase: "listening", request: "r", at: now}, now)
			e := nativePauseEvent{phase: "busy", request: "r", matched: true, final: true, at: now}
			switch kind {
			case "snapshot":
				e.initial = true
			case "stale":
				e.at = now.Add(-3 * time.Second)
			case "changed-sender":
				r.s.Client = "other"
			case "no-request":
				e.request = ""
			case "idle":
				e.phase = "idle"
			}
			if ok, _ := p.handle(ctx, e, now); ok || len(r.commands) != 0 {
				t.Fatal("unowned or stale command executed", kind)
			}
		})
	}
}
func TestNativeSpotterPauseDebounce(t *testing.T) {
	_, r, _ := testControls()
	p := pauseVoiceController{receiver: r}
	now := time.Now()
	ctx := context.Background()
	e := nativePauseEvent{phase: "recognition", matched: true, final: true, local: true, initial: true, at: now}
	if ok, _ := p.handle(ctx, e, now); ok {
		t.Fatal("initial local snapshot executed")
	}
	e.initial = false
	if ok, err := p.handle(ctx, e, now); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := p.handle(ctx, e, now.Add(100*time.Millisecond)); ok {
		t.Fatal("duplicate local event executed")
	}
}

func TestNativePauseRearmsForNextIdlessListeningState(t *testing.T) {
	_, r, _ := testControls()
	p := pauseVoiceController{receiver: r}
	ctx, now := context.Background(), time.Now()
	for _, id := range []string{"first", "second"} {
		_, _ = p.handle(ctx, nativePauseEvent{phase: "listening", at: now}, now)
		ok, err := p.handle(ctx, nativePauseEvent{phase: "busy", request: id, matched: true, final: true, at: now}, now)
		if !ok || err != nil {
			t.Fatal("next utterance not armed", id, err)
		}
		now = now.Add(time.Second)
	}
	if len(r.commands) != 2 {
		t.Fatal(r.commands)
	}
}
func TestNativeSpeechEndpointOnlyUsesConfiguredLoopbackPort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "quasar.cfg")
	if err := os.WriteFile(p, []byte(`{"aliced":{"port":9875},"private":"not exposed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if ep, err := nativeSpeechEndpoint(p); err != nil || ep != "127.0.0.1:9875" {
		t.Fatal(ep, err)
	}
	if err := os.WriteFile(p, []byte(`{"private":"not exposed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeSpeechEndpoint(p); err == nil {
		t.Fatal("missing endpoint accepted")
	}
}
func TestNativeSpeechFragmentedFramesAndCancellation(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan nativePauseEvent, 4)
	done := make(chan error, 1)
	go func() { done <- readNativePause(ctx, a, func(e nativePauseEvent) { events <- e }) }()
	for _, raw := range [][]byte{speechFixture(0, "", "", false, true), speechFixture(1, "r", "", false, true), speechFixture(3, "r", "пауза", true, true)} {
		frame := binary.LittleEndian.AppendUint32(nil, uint32(len(raw)))
		frame = append(frame, raw...)
		for len(frame) > 0 {
			n := 3
			if n > len(frame) {
				n = len(frame)
			}
			if _, err := b.Write(frame[:n]); err != nil {
				t.Fatal(err)
			}
			frame = frame[n:]
		}
	}
	for n := 0; n < 3; n++ {
		select {
		case e := <-events:
			if e.initial != (n == 0) {
				t.Fatal("snapshot marker wrong", n)
			}
		case <-time.After(time.Second):
			t.Fatal("fragmented event lost")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("speech reader outlived cancellation")
	}
}
func TestStandaloneNativePauseOptIn(t *testing.T) {
	if err := standaloneConfig(&controlsConfig{NativeVolume: true, VoicePauseOnly: true}); err != nil {
		t.Fatal(err)
	}
}

func TestNativePauseOptionRequiresStandalone(t *testing.T) {
	err := controlsPreflight(context.Background(), nil, yamDevice{}, &controlsConfig{VoicePauseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "standalone") {
		t.Fatalf("runtime host silently accepted standalone-only voice option: %v", err)
	}
}

func TestNativePauseRuntimeCannotSilentlyDropOption(t *testing.T) {
	err := runWithControls(context.Background(), true, &controlsConfig{VoicePauseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "standalone") {
		t.Fatalf("standalone option reached runtime discovery: %v", err)
	}
}
