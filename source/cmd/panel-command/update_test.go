package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateResult(t *testing.T) {
	for _, tc := range []struct {
		name, plan, install, after, post, want string
		stops                                  bool
	}{
		{"plan unavailable", "fail", "ok", "2.1.20", "ok", "plan_failed", false},
		{"empty plan", "empty", "ok", "2.1.20", "ok", "plan_failed", false},
		{"current", "2.1.19", "ok", "2.1.19", "ok", "current", false},
		{"install failure", "2.1.20", "fail", "2.1.19", "ok", "install_failed", true},
		{"stop failure", "2.1.20", "ok", "2.1.20", "stop_fail", "stop_failed", true},
		{"wrong version", "2.1.20", "ok", "2.1.19", "ok", "verify_failed", true},
		{"restore failure", "2.1.20", "ok", "2.1.20", "start_fail", "restore_failed", true},
		{"install and restore failure", "2.1.20", "fail", "2.1.19", "start_fail", "install_failed_restore_failed", true},
		{"interrupt before stop", "term", "ok", "2.1.19", "ok", "interrupted", false},
		{"interrupt first stop", "2.1.20", "ok", "2.1.19", "term", "interrupted", true},
		{"interrupt install", "2.1.20", "term", "2.1.19", "ok", "interrupted", true},
		{"interrupt and restore failure", "2.1.20", "term", "2.1.19", "start_fail", "interrupted_restore_failed", true},
		{"updated", "2.1.20", "ok", "2.1.20", "ok", "updated", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			stub := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			stub("yam", `
shift 4
case "$1" in
 list) if [ -f "$FIXTURE/installed" ]; then v=$AFTER; else v=2.1.19; fi; printf 'airplay/airplay-luna\t%s\n' "$v" ;;
 plan) case "$PLAN" in term) kill -TERM "$UPDATE_PID"; exit 1;; fail) exit 1;; empty) exit 0;; esac; printf 'airplay/airplay-luna\t%s\tregistry\n' "$PLAN" ;;
 install) printf 'install %s\n' "$3" >>"$FIXTURE/calls"; if [ "$INSTALL" = term ]; then kill -TERM "$UPDATE_PID"; exit 1; fi; [ "$INSTALL" = ok ] || exit 1; touch "$FIXTURE/installed" ;;
 *) exit 1 ;;
esac`)
			stub("wget", `for arg do url=$arg; done
printf '%s\n' "$url" >>"$FIXTURE/calls"
case "$url:$POST" in */airplay.panel/stop:term) kill -TERM "$UPDATE_PID"; exit 1;; */stop:stop_fail|*/start:start_fail) exit 1;; esac`)
			stub("pidof", "exit 1")
			result := filepath.Join(root, "result")
			cmd := exec.Command("/bin/sh", "-c", "export UPDATE_PID=$$\n"+updateBody, "update", root, "yandexmini", result)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FIXTURE="+root, "PLAN="+tc.plan, "INSTALL="+tc.install, "AFTER="+tc.after, "POST="+tc.post)
			out, err := cmd.CombinedOutput()
			if (err == nil) != (tc.want == "updated" || tc.want == "current") {
				t.Fatalf("exit: %v; %s", err, out)
			}
			got, e := os.ReadFile(result)
			if e != nil || strings.TrimSpace(string(got)) != tc.want {
				t.Fatalf("result=%q err=%v output=%s", got, e, out)
			}
			calls, _ := os.ReadFile(filepath.Join(root, "calls"))
			log := string(calls)
			if strings.Contains(log, "/stop") != tc.stops || strings.Contains(log, "/service/start") != tc.stops {
				t.Fatalf("calls:\n%s", log)
			}
			if tc.stops && (strings.Count(log, "/service/start") != 2 || strings.Count(log, "/service/enable") != 2) {
				t.Fatalf("incomplete or repeated restore: %s", log)
			}
			if tc.post == "stop_fail" && strings.Contains(log, "install ") {
				t.Fatalf("installed after failed stop: %s", log)
			}
			if tc.want == "updated" && !strings.Contains(log, "install airplay/airplay-luna@"+tc.plan) {
				t.Fatalf("did not install exact plan: %s", log)
			}
		})
	}
}
