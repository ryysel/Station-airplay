package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"
)

func TestOrionSetFrameHasVolumeSet(t *testing.T) {
	p := orionSetFrame(55)
	if len(p) < 8 {
		t.Fatal(p)
	}
	n := int(binary.LittleEndian.Uint32(p[:4]))
	if n != len(p)-4 {
		t.Fatalf("length %d body %d", n, len(p)-4)
	}
	if !containsBytes(p, []byte("volume_set")) {
		t.Fatal("missing volume_set")
	}
}

func TestOrionVolumeParsesRealFrame(t *testing.T) {
	body := orionVolumeBody(55, false)
	percent, present, err := orionVolume(body)
	if err != nil || !present || math.Abs(percent-55) > 0.01 {
		t.Fatalf("parse 55: percent=%v present=%v err=%v", percent, present, err)
	}
	muted, present, err := orionVolume(orionVolumeBody(40, true))
	if err != nil || !present || muted != 0 {
		t.Fatalf("muted: percent=%v present=%v err=%v", muted, present, err)
	}
	if _, present, err = orionVolume([]byte{8, 1}); err != nil || present {
		t.Fatalf("unrelated frame: present=%v err=%v", present, err)
	}
}

func TestOrionAcknowledgesSetOnOriginalStateSocket(t *testing.T) {
	listen := func() *net.TCPListener {
		t.Helper()
		ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		ln.SetDeadline(time.Now().Add(2 * time.Second))
		return ln
	}
	state, control := listen(), listen()
	v := orionNativeVolume{statePort: state.Addr().(*net.TCPAddr).Port, controlPort: control.Addr().(*net.TCPAddr).Port}
	type result struct {
		value float64
		err   error
	}
	done := make(chan result, 1)
	go func() { value, err := v.SetVolume(context.Background(), 54); done <- result{value, err} }()
	conn, err := state.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err = conn.Write(orionVolumeTCP(20, false)); err != nil {
		t.Fatal(err)
	}
	write, err := control.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer write.Close()
	write.SetDeadline(time.Now().Add(2 * time.Second))
	frame, err := orionFrame(write)
	if err != nil || !bytes.Equal(frame, orionSetFrame(55)[4:]) {
		t.Fatalf("set frame: %x (%v)", frame, err)
	}
	// A stale notification before the real ACK must not satisfy the request.
	if _, err = conn.Write(append(orionVolumeTCP(20, false), orionVolumeTCP(55, false)...)); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.value != 55 {
		t.Fatalf("readback: %v (%v)", got.value, got.err)
	}
}

func TestOrionSetVolumeTimesOutWithoutAck(t *testing.T) {
	stateLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stateLn.Close()
	controlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer controlLn.Close()
	go func() {
		c, err := stateLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write(orionVolumeTCP(20, false))
		time.Sleep(2 * time.Second)
	}()
	go func() {
		c, err := controlLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 256)
		_, _ = c.Read(buf)
		time.Sleep(2 * time.Second)
	}()
	v := &orionNativeVolume{statePort: stateLn.Addr().(*net.TCPAddr).Port, controlPort: controlLn.Addr().(*net.TCPAddr).Port}
	start := time.Now()
	_, err = v.SetVolume(context.Background(), 55)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("missing ACK succeeded")
	}
	if elapsed < 200*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("ACK wait %s", elapsed)
	}
}

func orionVolumeBody(percent float32, muted bool) []byte {
	var state []byte
	if muted {
		state = append(state, 24, 1) // field 3, varint 1
	} else {
		state = append(state, 24, 0)
	}
	state = append(state, 53) // field 6, wire 5
	var bits [4]byte
	binary.LittleEndian.PutUint32(bits[:], math.Float32bits(percent))
	state = append(state, bits[:]...)
	inner := protoBytes(11, state)
	body := protoBytes(1, inner)
	return protoBytes(198, body)
}

func orionVolumeTCP(percent float32, muted bool) []byte {
	body := orionVolumeBody(percent, muted)
	out := make([]byte, 4+len(body))
	binary.LittleEndian.PutUint32(out[:4], uint32(len(body)))
	copy(out[4:], body)
	return out
}

func protoBytes(field uint64, value []byte) []byte {
	var key [10]byte
	n := binary.PutUvarint(key[:], field<<3|2)
	var ln [10]byte
	m := binary.PutUvarint(ln[:], uint64(len(value)))
	out := make([]byte, 0, n+m+len(value))
	out = append(out, key[:n]...)
	out = append(out, ln[:m]...)
	return append(out, value...)
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
