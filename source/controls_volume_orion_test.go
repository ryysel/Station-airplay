package main

import (
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
