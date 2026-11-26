package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Only the Avahi child sees these accounts, through avahi-namespace. Neither
// the firmware account files nor the system bus policy need to be writable.
func preparePrivateMDNS(state, systemDir string) (string, error) {
	for _, file := range []struct{ name, addition string }{
		{"passwd", "avahi:x:70:70:Avahi mDNS:/run/station-airplay:/bin/false\n"},
		{"group", "avahi:x:70:\n"},
	} {
		b, err := os.ReadFile(filepath.Join(systemDir, file.name))
		if err != nil {
			return "", err
		}
		if !strings.Contains("\n"+string(b), "\navahi:") {
			b = append(b, []byte("\n"+file.addition)...)
		}
		if err = os.WriteFile(filepath.Join(state, file.name), b, 0600); err != nil {
			return "", err
		}
	}
	bus := filepath.Join(state, "private-bus")
	conf := fmt.Sprintf(`<busconfig><type>system</type><listen>unix:path=%s</listen><auth>EXTERNAL</auth><policy user="root"><allow own="*"/><allow send_destination="*"/><allow receive_sender="*"/></policy></busconfig>`, bus)
	return bus, os.WriteFile(filepath.Join(state, "bus.conf"), []byte(conf), 0600)
}

// launch owns and monitors each child before readiness probes start. Both
// audio engines use the same isolated mDNS stack, including on clean firmware.
func startPrivateMDNS(ctx context.Context, root, state string, p stationProfile, envFor func(string) []string, launch func(string, string, ...string) error, exits <-chan exitInfo) error {
	bus := filepath.Join(state, "private-bus")
	dbusBin, dbusArgs := dbusCommand(root, p, "dbus-daemon")
	if err := launch("dbus-daemon", dbusBin, append(dbusArgs, "--config-file="+filepath.Join(state, "bus.conf"), "--nofork")...); err != nil {
		return err
	}
	ready := func(stage string, check func(context.Context) error) error {
		return waitSharedStartup(ctx, stage, sharedStartupTimeout, sharedStartupInterval, exits, check)
	}
	if err := ready("private D-Bus socket", func(context.Context) error {
		fi, err := os.Stat(bus)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSocket == 0 {
			return errors.New("private bus path is not a socket")
		}
		return nil
	}); err != nil {
		return err
	}
	avahiConf, err := avahiConfig(root, state, p)
	if err != nil {
		return err
	}
	daemon, prefix := withLegacyRuntime(root, filepath.Join(root, "bin/avahi-daemon"), nil)
	avahiArgs := append([]string{state, daemon}, prefix...)
	avahiArgs = append(avahiArgs, "-f", avahiConf, "--no-drop-root", "--no-chroot", "--no-rlimits", "--no-proc-title")
	if err = launch("avahi-namespace", filepath.Join(root, "bin/avahi-namespace"), avahiArgs...); err != nil {
		return err
	}
	return ready("private Avahi D-Bus registration", func(parent context.Context) error {
		checkCtx, cancel := context.WithTimeout(parent, 3*time.Second)
		defer cancel()
		sendBin, sendArgs := dbusCommand(root, p, "dbus-send")
		sendBin, sendArgs = withLegacyRuntime(root, sendBin, sendArgs)
		c := exec.CommandContext(checkCtx, sendBin, append(sendArgs, "--system", "--print-reply", "--dest=org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus.NameHasOwner", "string:org.freedesktop.Avahi")...)
		c.Env = envFor(sendBin)
		b, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("Avahi NameHasOwner: %w: %.1024s", err, b)
		}
		if !strings.Contains(string(b), "boolean true") {
			return errors.New("Avahi has not acquired its D-Bus name")
		}
		return nil
	})
}
