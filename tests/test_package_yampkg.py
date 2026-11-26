#!/usr/bin/env python3
"""Validate the YAM v2 package matrix and rendered launchers."""
import importlib.util
import json
import pathlib
import shutil
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts/package_yampkg.py"
MATRIX = ROOT / "release/yampkg/targets.json"
TEMPLATE = ROOT / "release/yampkg/launch-airplay.sh.in"

spec = importlib.util.spec_from_file_location("package_yampkg", SCRIPT)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

matrix = module.load_matrix(MATRIX)
module.validate_matrix(matrix)
template = TEMPLATE.read_text()
module.check(matrix, template)
assert module.LUNA_DEPENDENCY == "yam/luna >=0.2.32"
plugin_toml = (ROOT / "plugin.toml").read_text()
assert "Требует Luna 0.2.32+." in plugin_toml
assert module.luna_requirement_text() == "Требует Luna 0.2.32+."
by_target = {target["target"]: target for target in matrix["targets"]}

assert set(by_target) == {
    "bergamot",
    "cucumber",
    "mango",
    "orion",
    "pickle",
    "plum",
    "yandexmicro",
    "yandexmidi",
    "yandexmini",
    "yandexmini_2",
    "yandexplum",
    "yandexstation_2",
}

# Midi and the original Mini receive the full control set.
for name in ("yandexmidi", "cucumber", "yandexmini"):
    assert by_target[name]["controls_config"], name
orion = by_target["orion"]
assert orion["controls_config"] and orion["dbus_export"]
assert orion["sleep"] == [1]
for setting in ("voice", "native_volume", "ducking"):
    assert orion["config"][setting] is True, setting
for setting in ("leds", "native_player", "alice_button", "spectrum"):
    assert orion["config"][setting] is False, setting
assert by_target["bergamot"]["controls_config"]
assert by_target["yandexmidi"]["config"]["native_player"]
assert by_target["yandexmidi"]["config"]["alice_button"]
assert by_target["cucumber"]["config"]["native_player"]
assert by_target["cucumber"]["config"]["alice_button"]
SPECTRUM = {
    "spectrum": True,
    "spectrum_brightness": 1.0,
    "spectrum_gain_r": 1.0,
    "spectrum_gain_g": 1.0,
    "spectrum_gain_b": 1.0,
    "led_color_profile": "",
}
# The Mini keeps a solid logo-light indication, so no artwork spectrum, and
# its Luna host does not advertise an Alice-button transport.
assert by_target["yandexmini"]["config"] == {
    "voice": True,
    "native_volume": True,
    "ducking": True,
    "leds": True,
    "led_level": 200,
    "native_player": True,
    "alice_button": False,
    **{**SPECTRUM, "spectrum": False},
}
assert by_target["yandexmini"]["models_append"] == ["YandexStationMini"]

full = {
    "voice": True,
    "native_volume": True,
    "ducking": True,
    "leds": True,
    "led_level": 200,
    "native_player": True,
    "alice_button": True,
}
lite_tuning = {
    "spectrum": True,
    "spectrum_brightness": 1.25,
    "spectrum_gain_r": 1.5,
    "spectrum_gain_g": 0.5,
    "spectrum_gain_b": 0.71,
    "led_color_profile": "",
}
mini2_tuning = {
    "spectrum": True,
    "spectrum_brightness": 1.25,
    "spectrum_gain_r": 1.0,
    "spectrum_gain_g": 1.0,
    "spectrum_gain_b": 1.0,
    "led_color_profile": "red",
}
assert by_target["bergamot"]["config"] == {**full, **SPECTRUM}
assert by_target["plum"]["config"] == {**full, **lite_tuning}
assert by_target["yandexplum"]["config"] == {**full, **lite_tuning}
assert by_target["yandexmini_2"]["config"] == {**full, **mini2_tuning}
# Station 2 now takes over its framebuffer ring for artwork and spectrum.
# Station 3 (orion) still keeps its native indication.
assert by_target["yandexstation_2"]["config"] == {**full, **SPECTRUM}
assert by_target["orion"]["config"]["spectrum"] is False
assert by_target["cucumber"]["config"]["spectrum"] is True
assert by_target["mango"]["config"]["spectrum"] is True
assert by_target["yandexmicro"]["config"] == {
    "voice": True,
    # The firmware does not apply the station level to the AirPlay dmix path,
    # so the receiver scales the sender's slider itself.
    "native_volume": False,
    "ducking": True,
    "leds": True,
    "led_level": 200,
    "native_player": False,
    "alice_button": False,
    **SPECTRUM,
}
assert by_target["mango"]["config"]["native_volume"] is False
assert by_target["mango"]["config"]["native_player"] is False
assert by_target["mango"]["config"]["alice_button"] is False

# Mini 3 Pro reuses the Mini 3 runtime but adds the AArch64 receiver assets.
assert not by_target["bergamot"].get("base_target")
assert not by_target["bergamot"].get("models_append")
assert not by_target["bergamot"].get("extra_files")
pickle = by_target["pickle"]
assert pickle["base_target"] == "bergamot"
assert pickle["controls_config"] is False
assert pickle["models_append"] == ["yandexmini_3_pro"]
assert set(pickle["extra_files"]) == {
    "bin/shairport-sync-ap2-aarch64",
    "lib/aarch64/libplist-2.0.so.3",
}
assert pickle["config"]["native_player"] is False
assert pickle["config"]["alice_button"] is False

for target in matrix["targets"]:
    launch = module.render_launcher(template, target)
    assert launch.startswith("#!/bin/sh\n")
    assert launch.count('exec "$here/bin/station-airplay"') == 1
    assert ('--controls-config "$config"' in launch) == target["controls_config"]
    assert "yandexmidi)" in launch
    assert ("DBUS_SYSTEM_BUS_ADDRESS" in launch) == target["dbus_export"]
    assert ("[ \"$model\" != \"mango\" ]" in launch) == target["mango_guard"]
    for value in target["sleep"]:
        assert ("sleep %d" % value) in launch
    config = json.loads(module.render_config(target))
    assert config == target["config"]
    if shutil.which("sh"):
        with tempfile.NamedTemporaryFile("w", suffix=".sh", delete=False) as handle:
            handle.write(launch)
            path = handle.name
        try:
            subprocess.run(["sh", "-n", path], check=True)
        finally:
            pathlib.Path(path).unlink()

# The clean-install enable hook must survive a missing Luna settings file.
postinstall = ROOT / "release/yampkg/airplay-postinstall.sh"
assert postinstall.is_file()
assert 'cat "$settings" 2>/dev/null || true' in postinstall.read_text()
if shutil.which("sh"):
    subprocess.run(["sh", "-n", str(postinstall)], check=True)

# `plum` must not steal `yandexplum.yampkg`; prefixed base names still work.
with tempfile.TemporaryDirectory(prefix="yampkg-base-") as directory:
    root = pathlib.Path(directory)
    (root / "plum.yampkg").write_bytes(b"plum")
    (root / "yandexplum.yampkg").write_bytes(b"plum2")
    (root / "station-airplay-2.1.0-yandexstation_2.yampkg").write_bytes(b"st2")
    assert module.find_base(root, "plum").name == "plum.yampkg"
    assert module.find_base(root, "yandexplum").name == "yandexplum.yampkg"
    assert (
        module.find_base(root, "yandexstation_2").name
        == "station-airplay-2.1.0-yandexstation_2.yampkg"
    )
    (root / "a-zeta.yampkg").write_bytes(b"a")
    (root / "b-zeta.yampkg").write_bytes(b"b")
    try:
        module.find_base(root, "zeta")
    except SystemExit:
        pass
    else:
        raise AssertionError("ambiguous base package accepted")

# The CLI must validate the matrix without a base package or PyYAML.
subprocess.run([sys.executable, str(SCRIPT), "--check"], cwd=ROOT, check=True)
print("YAM v2 package matrix, launchers and configs verified")
