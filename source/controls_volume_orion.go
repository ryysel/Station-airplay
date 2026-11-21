package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strconv"
	"time"
)

// Orion's audible and Alice volume is Quasar's volume_set, the same directive
// the buttons send. ALSA Master and set_volume.sh only move mixers; Quasar
// then overwrites them from its own level.
const orionQuasarConfig = "/system/vendor/quasar/quasar.cfg"

type orionNativeVolume struct {
	statePort   int
	controlPort int
}

type orionQuasarPorts struct {
	State struct {
		Port int `json:"port"`
	} `json:"stereo_pair"`
	Control struct {
		Port int `json:"port"`
	} `json:"iohub_clients"`
}

func newOrionAmpVolume() (*orionNativeVolume, error) {
	file, err := os.Open(orionQuasarConfig)
	if err != nil {
		return nil, err
	}
	var cfg orionQuasarPorts
	err = json.NewDecoder(file).Decode(&cfg)
	file.Close()
	if err != nil || cfg.State.Port == 0 || cfg.Control.Port == 0 {
		return nil, errors.New("orion volume IPC endpoints missing from quasar.cfg")
	}
	return &orionNativeVolume{statePort: cfg.State.Port, controlPort: cfg.Control.Port}, nil
}

func (v *orionNativeVolume) Backend() string { return "Orion Quasar volume_set" }

func (v *orionNativeVolume) CheckWrite(ctx context.Context) error {
	_, err := v.GetVolume(ctx)
	return err
}

func (v *orionNativeVolume) GetVolume(ctx context.Context) (float64, error) {
	s, err := orionDial(ctx, v.statePort)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	return orionReadVolume(ctx, s)
}

func (v *orionNativeVolume) SetVolume(ctx context.Context, pct int) (float64, error) {
	if pct < 0 || pct > 100 {
		return 0, errors.New("volume must be 0..100")
	}
	wanted := int(math.Round(float64(pct)/5)) * 5
	current, err := v.GetVolume(ctx)
	if err != nil {
		return 0, err
	}
	if math.Abs(current-float64(wanted)) < 0.01 {
		return float64(wanted), nil
	}
	control, err := orionDial(ctx, v.controlPort)
	if err != nil {
		return 0, err
	}
	defer control.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = control.SetWriteDeadline(deadline)
	} else {
		_ = control.SetWriteDeadline(time.Now().Add(5 * time.Second))
	}
	packet := orionSetFrame(wanted)
	if _, err = control.Write(packet); err != nil {
		return 0, fmt.Errorf("orion volume write: %w", err)
	}
	state, err := orionDial(ctx, v.statePort)
	if err != nil {
		return 0, err
	}
	defer state.Close()
	for {
		value, err := orionReadVolume(ctx, state)
		if err != nil {
			return 0, fmt.Errorf("orion volume did not acknowledge %d: %w", wanted, err)
		}
		if math.Abs(value-float64(wanted)) < 0.01 {
			return value, nil
		}
	}
}

func orionDial(ctx context.Context, port int) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("orion volume connection: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	return conn, nil
}

func orionReadVolume(ctx context.Context, conn net.Conn) (float64, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		frame, err := orionFrame(conn)
		if err != nil {
			return 0, err
		}
		percent, present, err := orionVolume(frame)
		if err != nil {
			return 0, err
		}
		if present {
			return percent, nil
		}
	}
}

func orionFrame(conn net.Conn) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	n := int(binary.LittleEndian.Uint32(header[:]))
	if n == 0 || n > 8*1024*1024 {
		return nil, errors.New("invalid orion IPC frame length")
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(conn, buf)
	return buf, err
}

func orionNext(data []byte, offset *int) (number, wire uint64, value []byte, integer uint64, err error) {
	key, n := binary.Uvarint(data[*offset:])
	if n <= 0 || key>>3 == 0 {
		err = errors.New("invalid orion protobuf field")
		return
	}
	*offset += n
	number, wire = key>>3, key&7
	size := 0
	switch wire {
	case 0:
		integer, n = binary.Uvarint(data[*offset:])
		if n <= 0 {
			err = errors.New("invalid orion protobuf integer")
			return
		}
		*offset += n
		return
	case 1:
		size = 8
	case 2:
		var length uint64
		length, n = binary.Uvarint(data[*offset:])
		if n <= 0 || length > uint64(len(data)) {
			err = errors.New("invalid orion protobuf length")
			return
		}
		*offset += n
		size = int(length)
	case 5:
		size = 4
	default:
		err = errors.New("unsupported orion protobuf wire type")
		return
	}
	if size > len(data)-*offset {
		err = errors.New("truncated orion protobuf field")
		return
	}
	value = data[*offset : *offset+size]
	*offset += size
	return
}

func orionBytesField(data []byte, field uint64) ([]byte, error) {
	for offset := 0; offset < len(data); {
		number, wire, value, _, err := orionNext(data, &offset)
		if err != nil {
			return nil, err
		}
		if number == field && wire == 2 {
			return value, nil
		}
	}
	return nil, nil
}

func orionVolume(frame []byte) (float64, bool, error) {
	var body []byte
	var kind uint64
	for offset := 0; offset < len(frame); {
		number, wire, value, integer, err := orionNext(frame, &offset)
		if err != nil {
			return 0, false, err
		}
		switch {
		case number == 198 && wire == 2:
			body, kind = value, 198
		case number == 252 && wire == 2:
			body = value
		case number == 253 && wire == 0:
			kind = integer
		}
	}
	if kind != 198 {
		return 0, false, nil
	}
	state, err := orionBytesField(body, 1)
	if err != nil {
		return 0, false, err
	}
	state, err = orionBytesField(state, 11)
	if err != nil || state == nil {
		return 0, false, err
	}
	muted, present := false, false
	var percent float64
	for offset := 0; offset < len(state); {
		number, wire, value, integer, err := orionNext(state, &offset)
		if err != nil {
			return 0, false, err
		}
		if number == 3 && wire == 0 {
			muted = integer != 0
		} else if number == 6 && wire == 5 {
			percent = float64(math.Float32frombits(binary.LittleEndian.Uint32(value)))
			present = true
		}
	}
	if !present {
		return 0, false, nil
	}
	if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
		return 0, false, errors.New("invalid orion volume percentage")
	}
	if muted {
		return 0, true, nil
	}
	return percent, true, nil
}

func orionSetFrame(percent int) []byte {
	var typed [7]byte
	typed[0] = 13
	binary.LittleEndian.PutUint32(typed[1:5], math.Float32bits(float32(percent)))
	typed[5], typed[6] = 24, 1
	var directive [64]byte
	d := directive[:0]
	d = append(d, 10, 9)
	d = append(d, "directive"...)
	d = append(d, 18, 10)
	d = append(d, "volume_set"...)
	d = append(d, 32, 1, 122, byte(len(typed)))
	d = append(d, typed[:]...)
	d = binary.AppendUvarint(d, 20<<3)
	d = binary.AppendUvarint(d, 254)
	var packet [96]byte
	p := packet[:4]
	ioControlLen := 3 + 2 + 2 + len(d)
	p = binary.AppendUvarint(p, 252<<3|2)
	p = binary.AppendUvarint(p, uint64(ioControlLen))
	p = binary.AppendUvarint(p, 95<<3|2)
	p = binary.AppendUvarint(p, uint64(4+len(d)))
	p = append(p, 10, byte(2+len(d)), 10, byte(len(d)))
	p = append(p, d...)
	p = binary.AppendUvarint(p, 253<<3)
	p = binary.AppendUvarint(p, 88)
	binary.LittleEndian.PutUint32(p[:4], uint32(len(p)-4))
	return p
}
