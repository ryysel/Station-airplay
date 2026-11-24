package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The detached shell inherits the lock and uses an attempt-scoped result. Both
// survive replacement of the helper executable during the YAM transaction.
func startUpdate(root, model, attempt string) string {
	dir := updateDir(root)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "launch_failed"
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "launch_failed"
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK {
			return "busy"
		}
		return "launch_failed"
	}
	if status := updateStatus(root, attempt); status != "unknown" {
		return status
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		id := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".log"), ".next")
		if info, err := entry.Info(); err == nil && validAttempt(id) && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
	result := filepath.Join(dir, attempt)
	if err := os.WriteFile(result, []byte("pending\n"), 0600); err != nil {
		return "launch_failed"
	}
	command := exec.Command("/bin/sh", "-c", updateBody, "airplay-update", root, model, result)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.ExtraFiles = []*os.File{lock}
	if err := command.Start(); err != nil {
		_ = os.WriteFile(result, []byte("launch_failed\n"), 0600)
		return "launch_failed"
	}
	_ = command.Process.Release()
	return "pending"
}

func updateDir(root string) string { return filepath.Join(root, "var", "airplay-update") }

func updateStatus(root, attempt string) string {
	data, err := os.ReadFile(filepath.Join(updateDir(root), attempt))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

// yamRoot finds the YAM tree from this executable's payload path. The plugin
// runs from <root>/pkgs/<owner>/<package>/<version>/share/..., which also works
// on devices that keep YAM outside /data.
func yamRoot() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for depth := 0; depth < 10; depth++ {
			if filepath.Base(dir) == "pkgs" {
				return filepath.Dir(dir)
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if root := lunaYamRoot(); root != "" {
		return root
	}
	return "/data/yam"
}

func lunaYamRoot() string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name[0] < '0' || name[0] > '9' {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(data), "\x00")
		if len(args) == 0 || filepath.Base(args[0]) != "luna" {
			continue
		}
		for i, arg := range args {
			if arg == "--yam-root" && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(arg, "--yam-root=") {
				return strings.TrimPrefix(arg, "--yam-root=")
			}
		}
	}
	return ""
}

func lunaModel() string {
	client := &http.Client{Timeout: 5 * time.Second}
	request, err := http.NewRequest("GET", "http://127.0.0.1:8099/api/device", nil)
	if err != nil {
		return ""
	}
	request.Header.Set("X-Luna-Local", "1")
	response, err := client.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	var device struct {
		Model string `json:"model"`
	}
	if json.NewDecoder(response.Body).Decode(&device) != nil {
		return ""
	}
	return device.Model
}

const updateBody = `#!/bin/sh
umask 077
ROOT=$1
MODEL=$2
RESULT=$3
LOG=$RESULT.log
URL=http://127.0.0.1:8099
outcome=interrupted
finish() { printf '%s\n' "$outcome" >"$RESULT.next" && mv -f "$RESULT.next" "$RESULT"; }
trap finish EXIT
trap 'exit 1' HUP INT TERM
log() { printf '%s\n' "$*" >>"$LOG"; }
post() { wget -q -O /dev/null -T 20 --header='X-Luna-Local: 1' --header='Content-Type: application/json' --post-data='{}' "$URL$1" 2>/dev/null; }
yam() { "$ROOT/bin/yam" --platform "$MODEL" --profiles "$ROOT/platforms" "$@"; }
version() { awk '$1=="airplay/airplay-luna" { print $2; exit }'; }
if [ -z "$MODEL" ] || [ ! -x "$ROOT/bin/yam" ]; then
  outcome=launch_failed
  exit 1
fi
outcome=plan_failed
installed_list=$(yam list 2>>"$LOG") || exit 1
plan=$(yam plan airplay/airplay-luna 2>>"$LOG") || exit 1
installed=$(printf '%s\n' "$installed_list" | version)
planned=$(printf '%s\n' "$plan" | version)
if [ -z "$installed" ] || [ -z "$planned" ]; then
  log 'Could not read installed and planned AirPlay versions.'
  exit 1
fi
if [ "$installed" = "$planned" ]; then
  outcome=current
  exit 0
fi

stop_failed=0
for plugin in airplay.panel airplay.panel-command airplay.airplay-luna; do
  post "/api/plugins/$plugin/stop" || stop_failed=1
done
i=0
while [ "$i" -lt 30 ] && pidof station-airplay >/dev/null 2>&1; do
  i=$((i+1)); sleep 1
done
if [ "$stop_failed" -ne 0 ] || pidof station-airplay >/dev/null 2>&1; then
  outcome=stop_failed
else
  outcome=install_failed
  retry=1
  while :; do
    output=$(yam install -y "airplay/airplay-luna@$planned" 2>&1)
    rc=$?
    log "$output"
    if [ "$rc" -eq 0 ]; then
      after=$(yam list 2>>"$LOG")
      if [ "$?" -eq 0 ] && [ "$(printf '%s\n' "$after" | version)" = "$planned" ]; then
        outcome=updated
      else
        outcome=verify_failed
      fi
      break
    fi
    case "$output" in
      *"another yam transaction is running"*)
        [ "$retry" -lt 3 ] || break
        retry=$((retry+1)); sleep 4 ;;
      *) break ;;
    esac
  done
fi
# Always request restoration after any stop, including a partial stop or
# failed installation; don't let it turn an installation failure into success.
restore_failed=0
for plugin in airplay.airplay-luna airplay.panel; do
  post "/api/plugins/$plugin/service/enable" || restore_failed=1
  post "/api/plugins/$plugin/service/start" || restore_failed=1
done
if [ "$restore_failed" -ne 0 ]; then
  if [ "$outcome" = updated ]; then outcome=restore_failed
  else outcome="${outcome}_restore_failed"; fi
fi
[ "$outcome" = updated ]
`
