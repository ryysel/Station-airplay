package main

// Mini 2 pause/stop only. Read the station's existing AliceState IPC stream;
// never follow logs, store transcripts, cancel arbitrary dialogs or resume
// Alice's own player. Outgoing transport stays on the existing AirPlay D-Bus.
import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const nativeSpeechLimit = 512 * 1024

type speechField struct {
	wire   uint64
	data   []byte
	number uint64
}

func speechFields(data []byte) (map[uint64]speechField, error) {
	fields := make(map[uint64]speechField)
	for len(data) > 0 {
		key, n := binary.Uvarint(data)
		if n <= 0 || key>>3 == 0 {
			return nil, errors.New("invalid speech protobuf key")
		}
		data = data[n:]
		f := speechField{wire: key & 7}
		switch f.wire {
		case 0:
			f.number, n = binary.Uvarint(data)
			if n <= 0 {
				return nil, errors.New("invalid speech protobuf integer")
			}
			data = data[n:]
		case 1, 2, 5:
			size := uint64(8)
			if f.wire == 5 {
				size = 4
			}
			if f.wire == 2 {
				size, n = binary.Uvarint(data)
				if n <= 0 {
					return nil, errors.New("invalid speech protobuf length")
				}
				data = data[n:]
			}
			if size > uint64(len(data)) {
				return nil, errors.New("truncated speech protobuf")
			}
			f.data, data = data[:int(size)], data[int(size):]
		default:
			return nil, errors.New("unsupported speech protobuf wire type")
		}
		fields[key>>3] = f
	}
	return fields, nil
}

type nativePauseEvent struct {
	phase, request                 string
	matched, final, local, initial bool
	at                             time.Time
}

func pauseOnlyPhrase(text string) bool {
	normal := strings.Join(normalizePhrase(text), " ")
	normal = strings.TrimPrefix(normal, "алиса ")
	return normal == "пауза" || normal == "стоп"
}

// Field numbers come from quasar-protos AliceState: state1, request_id7,
// request_state15 {request_text1, is_final2}. Response text/cards are ignored.
func decodeNativePause(raw []byte) (nativePauseEvent, bool, error) {
	var e nativePauseEvent
	f, err := speechFields(raw)
	if err != nil {
		return e, false, err
	}
	if body, ok := f[252]; ok && body.wire == 2 {
		kind, ok := f[253]
		if !ok || kind.wire != 0 {
			return e, false, errors.New("invalid speech envelope")
		}
		f = map[uint64]speechField{kind.number: body}
	}
	if remoting, ok := f[210]; ok && remoting.wire == 2 {
		r, err := speechFields(remoting.data)
		if err != nil {
			return e, false, err
		}
		if string(r[7].data) != "CommandSpotterCapability" {
			return e, false, nil
		}
		command, err := speechFields(r[15].data)
		if err != nil {
			return e, false, err
		}
		text := command[3].data
		if len(text) > 4096 || !utf8.Valid(text) {
			return e, false, errors.New("invalid command text")
		}
		e.phase, e.local, e.final, e.matched = "recognition", true, true, pauseOnlyPhrase(string(text))
		return e, true, nil
	}
	body, ok := f[3]
	if !ok || body.wire != 2 {
		return e, false, nil
	}
	s, err := speechFields(body.data)
	if err != nil {
		return e, false, err
	}
	e.phase = "unknown"
	if phase, ok := s[1]; ok && phase.wire == 0 {
		switch phase.number {
		case 0:
			e.phase = "idle"
		case 1:
			e.phase = "listening"
		case 2:
			e.phase = "speaking"
		case 3:
			e.phase = "busy"
		}
	}
	id := s[7].data
	if len(id) > 256 || !utf8.Valid(id) {
		return e, false, errors.New("invalid speech request identity")
	}
	e.request = string(id)
	if request, ok := s[15]; ok && request.wire == 2 {
		r, err := speechFields(request.data)
		if err != nil {
			return e, false, err
		}
		text := r[1].data
		if len(text) > 4096 || !utf8.Valid(text) {
			return e, false, errors.New("invalid speech request text")
		}
		e.final = r[2].wire == 0 && r[2].number == 1
		e.matched = pauseOnlyPhrase(string(text))
	} else if e.phase == "busy" {
		// Older firmware publishes the deprecated recognized_phrase only.
		// BUSY follows completed recognition; LISTENING partials never qualify.
		text := s[2].data
		if len(text) > 4096 || !utf8.Valid(text) {
			return e, false, errors.New("invalid legacy speech text")
		}
		e.matched, e.final = pauseOnlyPhrase(string(text)), true
	}
	if source, ok := s[10]; ok && source.wire == 2 {
		r, err := speechFields(source.data)
		if err != nil {
			return e, false, err
		}
		e.local = string(r[6].data) == "command"
	}
	return e, true, nil
}

func nativeSpeechEndpoint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var cfg struct {
		Alice struct {
			Port uint16 `json:"port"`
		} `json:"aliced"`
	}
	if err = json.NewDecoder(f).Decode(&cfg); err != nil || cfg.Alice.Port == 0 {
		return "", errors.New("aliced IPC endpoint unavailable")
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(cfg.Alice.Port))), nil
}

func readNativePause(ctx context.Context, conn net.Conn, emit func(nativePauseEvent)) error {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	buffer := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	initial, connected := true, time.Now()
	for ctx.Err() == nil {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := conn.Read(chunk)
		buffer = append(buffer, chunk[:n]...)
		for len(buffer) >= 4 {
			size := uint64(binary.LittleEndian.Uint32(buffer[:4]))
			if size > nativeSpeechLimit {
				return errors.New("speech IPC frame exceeds limit")
			}
			if uint64(len(buffer)) < 4+size {
				break
			}
			raw := buffer[4 : 4+int(size)]
			e, ok, parseErr := decodeNativePause(raw)
			if parseErr != nil {
				return parseErr
			}
			if ok {
				e.at = time.Now()
				e.initial = initial || e.local && time.Since(connected) < time.Second
				initial = false
				emit(e)
			}
			buffer = buffer[4+int(size):]
		}
		if len(buffer) > nativeSpeechLimit+4 {
			return errors.New("speech IPC buffer exceeds limit")
		}
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				continue
			}
			return err
		}
	}
	return ctx.Err()
}

func watchNativePause(ctx context.Context, endpoint string, events chan<- nativePauseEvent) {
	emit := func(e nativePauseEvent) {
		select {
		case events <- e:
		case <-ctx.Done():
		}
	}
	var lastError time.Time
	for ctx.Err() == nil {
		conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", endpoint)
		if err == nil {
			fmt.Println("voice pause: observing native Alice commands (pause/stop only)")
			err = readNativePause(ctx, conn, emit)
			_ = conn.Close()
		}
		emit(nativePauseEvent{phase: "unavailable", at: time.Now()})
		if err != nil && ctx.Err() == nil && time.Since(lastError) > 30*time.Second {
			fmt.Println("voice pause: speech IPC unavailable; reconnecting")
			lastError = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

type pauseVoiceController struct {
	receiver        controlReceiver
	client, request string
	armed           time.Time
	seen            map[string]time.Time
	lastLocal       time.Time
}

func (p *pauseVoiceController) handle(ctx context.Context, e nativePauseEvent, now time.Time) (bool, error) {
	if p.seen == nil {
		p.seen = make(map[string]time.Time)
	}
	for key, at := range p.seen {
		if now.Sub(at) > 30*time.Second {
			delete(p.seen, key)
		}
	}
	if e.initial {
		if e.request != "" && len(p.seen) < 64 {
			p.seen[e.request] = now
		}
		p.client, p.request, p.armed = "", "", time.Time{}
		return false, nil
	}
	if e.phase == "idle" || e.phase == "unavailable" || e.phase == "unknown" {
		p.client, p.request, p.armed = "", "", time.Time{}
		if e.phase == "idle" || !e.local {
			return false, nil
		}
	}
	if now.Sub(e.at) > 2*time.Second || now.Before(e.at) {
		return false, nil
	}
	if e.phase == "listening" && (p.armed.IsZero() || now.Sub(p.armed) > 15*time.Second || e.request != "" && p.request != "" && e.request != p.request) {
		s, err := p.receiver.State(ctx)
		if err != nil {
			return false, err
		}
		p.client, p.request, p.armed = "", "", time.Time{}
		if s.Client != "" && s.Available && (s.Active || s.Connected) {
			p.client, p.request, p.armed = s.Client, e.request, now
		}
	}
	if !e.final || !e.matched {
		return false, nil
	}
	if e.request != "" {
		if _, ok := p.seen[e.request]; ok {
			return false, nil
		}
	}
	if e.local {
		if now.Sub(p.lastLocal) < time.Second {
			return false, nil
		}
	} else {
		if e.request == "" || p.client == "" || now.Sub(p.armed) > 15*time.Second || p.request != "" && e.request != p.request {
			return false, nil
		}
		if _, ok := p.seen[e.request]; ok {
			return false, nil
		}
		if len(p.seen) >= 64 {
			return false, nil
		}
	}
	s, err := p.receiver.State(ctx)
	if err != nil {
		return false, err
	}
	if !s.Available || s.Client == "" || !(s.Active || s.Connected) || !e.local && s.Client != p.client {
		return false, nil
	}
	if e.local {
		p.lastLocal = now
	}
	if e.request != "" && len(p.seen) < 64 {
		p.seen[e.request] = now
	}
	p.client, p.request, p.armed = "", "", time.Time{}
	if err = p.receiver.Command(ctx, "Pause"); err != nil {
		return false, err
	}
	return true, nil
}
