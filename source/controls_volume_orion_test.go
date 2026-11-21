package main

import (
	"encoding/binary"
	"testing"
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
	if string(p) == "" || !containsBytes(p, []byte("volume_set")) {
		t.Fatal("missing volume_set")
	}
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
