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

# The next package enables controls on Midi and restores Mini volume-only.
for name in ("yandexmidi", "cucumber", "yandexmini"):
    assert by_target[name]["controls_config"], name
assert not by_target["orion"]["controls_config"]
assert by_target["bergamot"]["controls_config"]
assert by_target["yandexmidi"]["config"]["native_player"]
assert by_target["yandexmidi"]["config"]["alice_button"]
assert by_target["cucumber"]["config"]["native_player"]
assert by_target["cucumber"]["config"]["alice_button"]
assert by_target["yandexmini"]["config"] == {
    "voice": False,
    "native_volume": True,
    "ducking": False,
    "leds": False,
    "led_level": 200,
    "native_player": False,
    "alice_button": False,
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
for name in ("bergamot", "plum", "yandexplum", "yandexmini_2", "yandexstation_2"):
    assert by_target[name]["config"] == full, name
assert by_target["yandexmicro"]["config"] == {
    "voice": True,
    "native_volume": True,
    "ducking": True,
    "leds": True,
    "led_level": 200,
    "native_player": False,
    "alice_button": False,
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
