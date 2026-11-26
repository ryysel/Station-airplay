package main

import (
	"encoding/json"
	"fmt"
)

// startPackageUpdate handles the panel's update button. It is a separate,
// deliberately narrow payload so the receiver RPC stays unchanged.
func startPackageUpdate(raw string) bool {
	var request struct {
		UpdateStatus bool   `json:"update_status"`
		Update       bool   `json:"update"`
		Attempt      string `json:"attempt"`
	}
	if json.Unmarshal([]byte(raw), &request) != nil || request.Update == request.UpdateStatus || !validAttempt(request.Attempt) {
		return false
	}
	root := yamRoot()
	status := updateStatus(root, request.Attempt)
	if request.Update && status == "unknown" {
		status = startUpdate(root, lunaModel(), request.Attempt)
	}
	fmt.Printf("\nAPU1 %s %s\n", request.Attempt, status)
	return true
}

func validAttempt(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
