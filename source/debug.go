package main

// Diagnostic log for the Alice button handover. The receiver deployment is
// read-only apart from /tmp and the plugin data directory, so the log lives in
// /tmp. It uses the same 128 KiB cap as receiver statistics.

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const aliceButtonDebugPath = "/tmp/station-airplay-button.log"

var (
	buttonLogOnce sync.Once
	buttonLog     *boundedLog
)

func aliceButtonDebug(format string, args ...any) {
	buttonLogOnce.Do(func() {
		_ = os.Remove(aliceButtonDebugPath)
		f, err := os.OpenFile(aliceButtonDebugPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			buttonLog = &boundedLog{f: f}
		}
	})
	if buttonLog != nil {
		_, _ = fmt.Fprintf(buttonLog, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
	}
}
