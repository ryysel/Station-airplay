#!/usr/bin/env python3
"""Rebuild YAM v2 per-target .yampkg packages from a published base release.

The public tree builds the legacy universal .yap only.  YAM v2 packages carry a
Luna launcher and a seeded controls JSON on top of the same runtime.  This tool
patches those files in a published base package, replaces the supervisor and
recomputes the manifest hashes, so the result is reviewable and reproducible.

It never signs or publishes anything.  Use yam-publish separately.
"""
import argparse
import hashlib
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from runtime_slim import RUNTIME_BINARIES, slim_file, upx_version  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
PLUGIN_REL = Path("files/share/luna/plugins/airplay.airplay-luna")
MANIFEST = "MANIFEST.yaml"
# Distribution archives use a high zstd level: the payload is mostly ELF and
# HTML/TOML, and decompression stays a single standard zstd stream.
PACK_ZSTD = "zstd -19 -T0 --zstd=wlog=20"
CONFIG_KEYS = (
    "voice",
    "native_volume",
    "ducking",
    "leds",
    "led_level",
    "native_player",
    "alice_button",
    "spectrum",
    "spectrum_brightness",
    "spectrum_gain_r",
    "spectrum_gain_g",
    "spectrum_gain_b",
    "led_color_profile",
)
BOOL_KEYS = (
    "voice",
    "native_volume",
    "ducking",
    "leds",
    "native_player",
    "alice_button",
    "spectrum",
)
NEW_PERMISSIONS = (
    'permissions = ["exec", "clock", "led", "ipc.speech", "commands", '
    '"media.native", "input.alice"]'
)
OLD_PERMISSIONS = 'permissions = ["exec", "clock", "led", "ipc.speech", "commands"]'
LUNA_DEPENDENCY = "yam/luna >=0.2.32"
# The public base release still ships its own loader, libc and glibc runtime
# libraries. The 2.1.5 runtime is built for the firmware glibc, so the package
# no longer needs a second libc; the supervisor points children at lib/.
PRUNE_RUNTIME = (
    "lib/ld-linux-armhf.so.3",
    "lib/libasound.so.2",
    "lib/libmount.so.1",
    "lib/libblkid.so.1",
    "lib/libselinux.so.1",
    "lib/libpcre2-8.so.0",
    "lib/libc.so.6",
    "lib/libm.so.6",
    "lib/libdl.so.2",
    "lib/libpthread.so.0",
    "lib/librt.so.1",
    "lib/libresolv.so.2",
    "lib/libgcc_s.so.1",
)
REPLACE_RUNTIME = (
    "bin/shairport-sync-ap2",
    "bin/dbus-daemon",
    "bin/dbus-send",
    "bin/nqptp",
    "bin/avahi-namespace",
    "lib/libdbus-1.so.3",
    "lib/libplist-2.0.so.3",
)
LEGACY_TARGETS = frozenset({"yandexmini"})
# Kept from the verified base payload, never installed into the firmware /lib.
LEGACY_RUNTIME = (
    "lib/ld-linux-armhf.so.3",
    "lib/libc.so.6",
    "lib/libm.so.6",
    "lib/libdl.so.2",
    "lib/libpthread.so.0",
    "lib/librt.so.1",
    "lib/libresolv.so.2",
    "lib/libgcc_s.so.1",
)


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def sha256_file(path):
    return sha256_bytes(Path(path).read_bytes())


def load_matrix(path):
    data = json.loads(Path(path).read_text())
    if data.get("schema") != 1:
        raise SystemExit("unsupported targets matrix schema")
    return data


def validate_matrix(matrix):
    targets = matrix.get("targets")
    if not isinstance(targets, list) or not targets:
        raise SystemExit("targets matrix is empty")
    names = set()
    for target in targets:
        name = target.get("target", "")
        if not name or "/" in name or name in names:
            raise SystemExit("bad or duplicate target %r" % name)
        names.add(name)
        if not isinstance(target.get("controls_config"), bool):
            raise SystemExit("%s: controls_config must be a boolean" % name)
        if not target["controls_config"] and target.get("mango_guard"):
            raise SystemExit("%s: mango_guard is only for targets that run controls" % name)
        sleep = target.get("sleep")
        if not isinstance(sleep, list) or not sleep or not all(
            isinstance(v, int) and 0 < v <= 60 for v in sleep
        ):
            raise SystemExit("%s: sleep must be a list of 1..60 second values" % name)
        if not isinstance(target.get("dbus_export"), bool):
            raise SystemExit("%s: dbus_export must be a boolean" % name)
        if not isinstance(target.get("mango_guard"), bool):
            raise SystemExit("%s: mango_guard must be a boolean" % name)
        base_target = target.get("base_target", name)
        if (
            not isinstance(base_target, str)
            or not base_target
            or "/" in base_target
            or ".." in Path(base_target).parts
        ):
            raise SystemExit("%s: invalid base_target" % name)
        models_append = target.get("models_append", [])
        if not isinstance(models_append, list) or not all(
            isinstance(model, str) and model for model in models_append
        ):
            raise SystemExit("%s: models_append must be a list of model IDs" % name)
        if len(set(models_append)) != len(models_append):
            raise SystemExit("%s: duplicate models_append entry" % name)
        extra_files = target.get("extra_files", {})
        if not isinstance(extra_files, dict):
            raise SystemExit("%s: extra_files must be an object" % name)
        for source, destination in extra_files.items():
            for value in (source, destination):
                if not isinstance(value, str) or not value:
                    raise SystemExit("%s: extra_files paths must be strings" % name)
                path = Path(value)
                if path.is_absolute() or ".." in path.parts:
                    raise SystemExit("%s: unsafe extra_files path %s" % (name, value))
        config = target.get("config")
        if not isinstance(config, dict) or set(config) != set(CONFIG_KEYS):
            raise SystemExit("%s: config must contain exactly %s" % (name, ", ".join(CONFIG_KEYS)))
        if not all(isinstance(config[key], bool) for key in BOOL_KEYS):
            raise SystemExit("%s: config feature flags must be booleans" % name)
        if not isinstance(config["led_level"], int) or not 0 <= config["led_level"] <= 255:
            raise SystemExit("%s: led_level must be 0..255" % name)
        brightness = config["spectrum_brightness"]
        if isinstance(brightness, bool) or not isinstance(brightness, (int, float)) or not 0 <= brightness <= 2:
            raise SystemExit("%s: spectrum_brightness must be 0..2" % name)
        for key in ("spectrum_gain_r", "spectrum_gain_g", "spectrum_gain_b"):
            gain = config[key]
            if isinstance(gain, bool) or not isinstance(gain, (int, float)) or not 0 <= gain <= 4:
                raise SystemExit("%s: %s must be 0..4" % (name, key))
        if config["led_color_profile"] not in ("", "red", "blue"):
            raise SystemExit("%s: led_color_profile must be empty, red or blue" % name)


def render_launcher(template, target):
    text = template
    text = text.replace(
        "@@MANGO_GUARD@@",
        ' && [ "$model" != "mango" ]' if target["mango_guard"] else "",
    )
    if target["dbus_export"]:
        text = text.replace(
            "@@DBUS_EXPORT@@\n",
            "export DBUS_SYSTEM_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket\n",
        )
    else:
        text = text.replace("@@DBUS_EXPORT@@\n", "")
    text = text.replace(
        "@@SLEEP_LINES@@\n", "".join("sleep %d\n" % value for value in target["sleep"])
    )
    text = text.replace(
        "@@CONTROLS_CONFIG@@",
        ' --controls-config "$config"' if target["controls_config"] else "",
    )
    if "@@" in text:
        raise SystemExit("%s: unrendered launcher placeholder" % target["target"])
    return text


def render_config(target):
    config = target["config"]
    ordered = {key: config[key] for key in CONFIG_KEYS}
    return json.dumps(ordered, indent=2) + "\n"


def check(matrix, template):
    for placeholder in (
        "@@MANGO_GUARD@@",
        "@@DBUS_EXPORT@@\n",
        "@@SLEEP_LINES@@\n",
        "@@CONTROLS_CONFIG@@",
    ):
        if placeholder not in template:
            raise SystemExit("launcher template lacks %s" % placeholder.strip("@\n"))
    for target in matrix["targets"]:
        launch = render_launcher(template, target)
        if not launch.startswith("#!/bin/sh\n"):
            raise SystemExit("%s: launcher must start with #!/bin/sh" % target["target"])
        if launch.count('exec "$here/bin/station-airplay"') != 1:
            raise SystemExit("%s: launcher must exec the supervisor once" % target["target"])
        if ('--controls-config "$config"' in launch) != target["controls_config"]:
            raise SystemExit("%s: launcher/config mismatch" % target["target"])
        json.loads(render_config(target))
    print("matrix OK: %d targets" % len(matrix["targets"]))


def _yaml():
    try:
        import yaml
    except ImportError:
        raise SystemExit(
            "PyYAML is required to build packages: "
            "python3 -m pip install -r release/yampkg/requirements.txt"
        )
    return yaml


def extract_package(package, destination):
    raw = subprocess.check_output(["zstd", "-dc", str(package)])
    with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
        for member in archive.getmembers():
            if not (member.isfile() or member.isdir()):
                raise SystemExit("%s: unsupported tar member %s" % (package, member.name))
            if member.name.startswith("/") or ".." in Path(member.name).parts:
                raise SystemExit("%s: unsafe tar member %s" % (package, member.name))
        try:
            archive.extractall(destination, filter="data")
        except TypeError:
            archive.extractall(destination)


def verify_base(stage, manifest):
    for relative, expected in manifest["files_sha256"].items():
        actual = sha256_file(Path(stage) / "files" / relative)
        if actual != expected:
            raise SystemExit("base package hash mismatch: %s" % relative)


def patch_plugin(path, base_version, version, models_append=()):
    text = Path(path).read_text()
    old = 'version = "%s"' % base_version
    if text.count(old) != 1:
        raise SystemExit("%s: expected one version line" % path)
    text = text.replace(old, 'version = "%s"' % version, 1)
    if '"media.native"' not in text or '"input.alice"' not in text:
        if text.count(OLD_PERMISSIONS) != 1:
            raise SystemExit("%s: cannot add narrow permissions" % path)
        text = text.replace(OLD_PERMISSIONS, NEW_PERMISSIONS, 1)
    if models_append:
        match = re.search(r"^models = (\[.*\])$", text, re.MULTILINE)
        if not match:
            raise SystemExit("%s: cannot find compatibility models" % path)
        models = json.loads(match.group(1))
        for model in models_append:
            if model not in models:
                models.append(model)
        text = text[: match.start(1)] + json.dumps(models) + text[match.end(1) :]
    Path(path).write_text(text)


def strip_voice_commands(path):
    """Models with voice disabled do not need the 214 KB phrase declaration."""
    path = Path(path)
    text = path.read_text()
    text, count = re.subn(r"\n\[commands\]\nphrases = \[.*?\]\n", "\n", text, flags=re.S)
    if count != 1:
        raise SystemExit("%s: expected one [commands].phrases block" % path)
    if '"commands", ' not in text:
        raise SystemExit("%s: commands permission missing" % path)
    path.write_text(text.replace('"commands", ', "", 1))


def extra_file_relative(destination):
    return str(PLUGIN_REL / destination)[len("files/") :]


def luna_requirement_text():
    version = LUNA_DEPENDENCY.split(">=", 1)[-1].strip()
    return "Требует Luna %s+." % version


def patch_manifest(manifest, target, version, stage, extra_files=(), description=None):
    manifest["target"] = target
    manifest["version"] = version
    depends = [item for item in manifest.get("depends", []) if not item.startswith("yam/luna")]
    depends.append(LUNA_DEPENDENCY)
    manifest["depends"] = depends
    if description is not None:
        manifest["description"] = description
    if luna_requirement_text() not in manifest.get("description", ""):
        raise SystemExit("%s: manifest description does not state %s" % (target, luna_requirement_text()))
    for destination in extra_files:
        relative = extra_file_relative(destination)
        path = Path(stage) / "files" / relative
        if not path.is_file():
            raise SystemExit("extra file missing: %s" % destination)
        manifest["files_sha256"][relative] = sha256_file(path)
    files = Path(stage) / "files"
    manifest["files_sha256"] = {
        path.relative_to(files).as_posix(): sha256_file(path)
        for path in sorted(files.rglob("*"))
        if path.is_file()
    }
    for relative in (
        str(PLUGIN_REL / "launch-airplay.sh")[len("files/") :],
        str(PLUGIN_REL / "config/airplay-controls-v2.json")[len("files/") :],
    ):
        if relative not in manifest["files_sha256"]:
            raise SystemExit("base manifest lacks %s" % relative)


def verify_archive_payload(archive, manifest):
    """Check both directions of the manifest and the executable tar modes."""
    members = {}
    for member in archive.getmembers():
        name = member.name.removeprefix("./")
        if name.startswith("/") or ".." in Path(name).parts:
            raise SystemExit("unsafe output member: %s" % name)
        if not (member.isfile() or member.isdir()):
            raise SystemExit("unsupported output member: %s" % name)
        if member.isdir():
            continue
        if name in members:
            raise SystemExit("duplicate output member: %s" % name)
        members[name] = member
    payload = {
        name[len("files/"):]: member
        for name, member in members.items() if name.startswith("files/")
    }
    hashes = manifest["files_sha256"]
    if set(payload) != set(hashes):
        raise SystemExit("payload/manifest mismatch: unlisted=%s missing=%s" % (
            sorted(set(payload) - set(hashes)), sorted(set(hashes) - set(payload))))
    plugin = str(PLUGIN_REL)[len("files/"):] + "/"
    for name, member in payload.items():
        if sha256_bytes(archive.extractfile(member).read()) != hashes[name]:
            raise SystemExit("output hash mismatch: %s" % name)
        executable = (
            name.startswith("bin/") or name.startswith(plugin + "bin/")
            or name == plugin + "launch-airplay.sh"
            or name == plugin + "lib/ld-linux-armhf.so.3"
        )
        if executable and member.mode & 0o111 != 0o111:
            raise SystemExit("output executable mode missing: %s (%o)" % (name, member.mode))


def verify_output(package, target, binary, version, launcher):
    yaml = _yaml()
    raw = subprocess.check_output(["zstd", "-dc", str(package)])
    with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
        manifest = yaml.safe_load(archive.extractfile(MANIFEST))
        if manifest["version"] != version or manifest["target"] != target["target"]:
            raise SystemExit("%s: manifest version/target mismatch" % package)
        if LUNA_DEPENDENCY not in manifest["depends"]:
            raise SystemExit("%s: manifest dependency missing" % package)
        if luna_requirement_text() not in manifest.get("description", ""):
            raise SystemExit("%s: manifest description does not state %s" % (package, luna_requirement_text()))
        verify_archive_payload(archive, manifest)
        if target["target"] in LEGACY_TARGETS:
            for relative in LEGACY_RUNTIME:
                if runtime_key(relative) not in manifest["files_sha256"]:
                    raise SystemExit("%s: missing legacy runtime %s" % (package, relative))
            for name in (*RUNTIME_BINARIES, "avahi-namespace"):
                data = archive.extractfile(str(PLUGIN_REL / "bin" / name)).read()
                if b"UPX!" in data[:4096]:
                    raise SystemExit("%s: Mini runtime must be unpacked: %s" % (package, name))
        payload = archive.extractfile(str(PLUGIN_REL / "bin/station-airplay")).read()
        if payload != binary:
            raise SystemExit("%s: supervisor payload mismatch" % package)
        if payload[:4] != b"\x7fELF" or payload[4] != 1:
            raise SystemExit("%s: supervisor is not ELF32" % package)
        if int.from_bytes(payload[18:20], "little") != 40:
            raise SystemExit("%s: supervisor is not ARM" % package)
        extracted = archive.extractfile(str(PLUGIN_REL / "launch-airplay.sh")).read().decode()
        if extracted != launcher:
            raise SystemExit("%s: launcher mismatch" % package)
        if ('--controls-config "$config"' in extracted) != target["controls_config"]:
            raise SystemExit("%s: launcher controls flag mismatch" % package)
        config = json.loads(
            archive.extractfile(str(PLUGIN_REL / "config/airplay-controls-v2.json")).read()
        )
        if config != target["config"]:
            raise SystemExit("%s: seeded config mismatch" % package)
        plugin = archive.extractfile(str(PLUGIN_REL / "plugin.toml")).read().decode()
        if 'version = "%s"' % version not in plugin:
            raise SystemExit("%s: plugin version mismatch" % package)
        if not target["config"]["voice"]:
            if "\n[commands]\n" in plugin or '"commands"' in plugin:
                raise SystemExit("%s: voice-disabled package still declares commands" % package)
        elif "\n[commands]\nphrases = [" not in plugin:
            raise SystemExit("%s: voice package lacks phrases" % package)
        for model in target.get("models_append", []):
            if '"%s"' % model not in plugin:
                raise SystemExit("%s: compatibility model missing: %s" % (package, model))
        members = set(archive.getnames())
        postinstall_src = ROOT / "release/yampkg/airplay-postinstall.sh"
        if postinstall_src.is_file():
            if "files/bin/airplay-postinstall" not in members:
                raise SystemExit("%s: postinstall missing" % package)
            if archive.extractfile("files/bin/airplay-postinstall").read() != postinstall_src.read_bytes():
                raise SystemExit("%s: postinstall mismatch" % package)
        for destination in target.get("extra_files", {}):
            if str(PLUGIN_REL / destination) not in members:
                raise SystemExit("%s: extra file missing: %s" % (package, destination))


_SLIM_CACHE = {}


def runtime_key(relative):
    return str(PLUGIN_REL / relative)[len("files/"):]


def copy_payload_file(source, destination, executable=False):
    """Source checkout permissions must not determine release executable modes."""
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, destination)
    os.chmod(destination, 0o755 if executable else 0o644)


def reconcile_runtime(plugin, manifest, target):
    """Retain Mini's verified base libc; slim only firmware-compatible targets."""
    legacy = target["target"] in LEGACY_TARGETS
    if legacy:
        for relative in LEGACY_RUNTIME:
            path = plugin / relative
            expected = manifest["files_sha256"].get(runtime_key(relative))
            if not path.is_file() or expected is None or sha256_file(path) != expected:
                raise SystemExit("missing or unverified Mini legacy runtime: %s" % relative)
    for relative in REPLACE_RUNTIME:
        source = ROOT / relative
        if not source.is_file():
            raise SystemExit("missing rebuilt runtime file: %s" % source)
        destination = plugin / relative
        copy_payload_file(source, destination, executable=relative.startswith("bin/"))
    for relative in PRUNE_RUNTIME:
        if legacy and relative in LEGACY_RUNTIME:
            continue
        path = plugin / relative
        if path.exists():
            path.unlink()
        manifest["files_sha256"].pop(runtime_key(relative), None)


def slim_runtime(plugin, executable="upx"):
    """Pack the receiver and daemons in this staged package; cache by input hash."""
    records = []
    for name in RUNTIME_BINARIES:
        path = plugin / "bin" / name
        if not path.is_file():
            continue
        key = (sha256_file(path), executable)
        if key in _SLIM_CACHE:
            packed, record = _SLIM_CACHE[key]
            path.write_bytes(packed)
            path.chmod(0o755)
            records.append(dict(record))
        else:
            record = slim_file(path, executable)
            _SLIM_CACHE[key] = (path.read_bytes(), record)
            records.append(record)
    return records


def pack_runtime_for_target(plugin, target, executable="upx"):
    # UPX removes the dynamic section that an explicit ld-linux invocation
    # needs. Mini's private loader must see the original dynamic ELF files.
    if target["target"] in LEGACY_TARGETS:
        for name in (*RUNTIME_BINARIES, "avahi-namespace"):
            path = plugin / "bin" / name
            if path.is_file() and b"UPX!" in path.read_bytes()[:4096]:
                raise SystemExit("Mini legacy runtime must be unpacked: %s" % name)
        return []
    return slim_runtime(plugin, executable)


def find_base(base_dir, name):
    exact = Path(base_dir) / ("%s.yampkg" % name)
    if exact.is_file():
        return exact
    matches = sorted(Path(base_dir).glob("*-%s.yampkg" % name))
    if len(matches) != 1:
        raise SystemExit(
            "%s: expected exactly one base package, found %d" % (name, len(matches))
        )
    return matches[0]


# Last Orion release with the complete ARMhf runtime. Do not copy firmware files.
ORION_RUNTIME_SHA256 = "9a624b3801cab60ce85aada19383ee692d1a5c558013089248cbaf3d7e298426"


def prepare_orion_runtime(plugin, source):
    if not source or sha256_file(source) != ORION_RUNTIME_SHA256:
        raise SystemExit("Orion requires --orion-runtime with the verified v2.0.9 Orion package")
    raw = subprocess.check_output(["zstd", "-dc", str(source)])
    compat = plugin / "compat-armhf"
    compat.mkdir(exist_ok=True)
    with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
        for relative in PRUNE_RUNTIME:
            path = compat / Path(relative).name
            path.write_bytes(archive.extractfile(str(PLUGIN_REL / relative)).read())
            path.chmod(0o755 if path.name.startswith("ld-linux") else 0o644)
    # An explicit dynamic loader cannot run UPX-packed children.
    for name in (*RUNTIME_BINARIES, "avahi-namespace"):
        copy_payload_file(ROOT / "bin" / name, plugin / "bin" / name, executable=True)


def build_target(
    target, base_dir, binary, version, out_dir, force, template, extra_dir,
    slim=False, upx="upx", orion_runtime=None,
):
    base_name = target.get("base_target", target["target"])
    base = find_base(base_dir, base_name)
    stage = tempfile.mkdtemp(prefix="yampkg-%s-" % target["target"])
    try:
        extract_package(base, stage)
        manifest_path = Path(stage) / MANIFEST
        yaml = _yaml()
        manifest = yaml.safe_load(manifest_path.read_text())
        if manifest.get("target") != base_name:
            raise SystemExit("%s: base target mismatch" % base)
        base_version = manifest.get("version")
        if not base_version:
            raise SystemExit("%s: base version missing" % base)
        verify_base(stage, manifest)

        plugin = Path(stage) / PLUGIN_REL
        (plugin / "bin/station-airplay").write_bytes(binary)
        os.chmod(plugin / "bin/station-airplay", 0o755)
        packed = []
        if slim:
            reconcile_runtime(plugin, manifest, target)
            if target["target"] != "orion":
                packed = pack_runtime_for_target(plugin, target, upx)
        if target["target"] == "orion":
            prepare_orion_runtime(plugin, orion_runtime)
        postinstall_src = ROOT / "release/yampkg/airplay-postinstall.sh"
        if postinstall_src.is_file():
            postinstall_dst = Path(stage) / "files/bin/airplay-postinstall"
            postinstall_dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(postinstall_src, postinstall_dst)
            os.chmod(postinstall_dst, 0o755)
            manifest["files_sha256"]["bin/airplay-postinstall"] = sha256_file(
                postinstall_dst
            )
        launcher = render_launcher(template, target)
        launcher_path = plugin / "launch-airplay.sh"
        launcher_path.write_text(launcher)
        os.chmod(launcher_path, 0o755)
        (plugin / "config/airplay-controls-v2.json").write_text(render_config(target))
        extra_files = target.get("extra_files", {})
        for source, destination in extra_files.items():
            if not extra_dir:
                raise SystemExit("%s: --extra-dir is required" % target["target"])
            source_path = Path(extra_dir) / source
            if not source_path.is_file():
                raise SystemExit("%s: missing extra file %s" % (target["target"], source))
            destination_path = plugin / destination
            copy_payload_file(source_path, destination_path, executable=destination.startswith("bin/"))
        patch_plugin(
            plugin / "plugin.toml",
            base_version,
            version,
            target.get("models_append", []),
        )
        if not target["config"]["voice"]:
            strip_voice_commands(plugin / "plugin.toml")
        description = default_description()
        if target["target"] in LEGACY_TARGETS:
            description += " Приватные loader/libc для оригинальной Mini (прошивка glibc 2.23)."
        patch_manifest(
            manifest,
            target["target"],
            version,
            stage,
            extra_files.values(),
            description=description,
        )
        manifest_path.write_text(
            yaml.safe_dump(manifest, sort_keys=False, allow_unicode=True)
        )

        out_dir.mkdir(parents=True, exist_ok=True)
        out = out_dir / ("station-airplay-%s-%s.yampkg" % (version, target["target"]))
        if out.exists() and not force:
            raise SystemExit("%s exists; pass --force to replace it" % out)
        subprocess.run(
            ["tar", "-I", PACK_ZSTD, "-cf", str(out), MANIFEST, "files"],
            cwd=stage,
            check=True,
        )
        verify_output(out, target, binary, version, launcher)
        return {
            "target": target["target"],
            "base_target": base_name,
            "version": version,
            "controls_config": target["controls_config"],
            "package": out.name,
            "sha256": sha256_file(out),
            "size": out.stat().st_size,
            "base_sha256": sha256_file(base),
            "binary_sha256": sha256_bytes(binary),
            "slim_runtime": packed,
            "legacy_runtime": {
                relative: sha256_file(plugin / relative)
                for relative in LEGACY_RUNTIME
                if target["target"] in LEGACY_TARGETS
            },
        }
    finally:
        shutil.rmtree(stage)


def default_version():
    text = (ROOT / "plugin.toml").read_text()
    match = re.search(r'^version = "([^"]+)"', text, re.MULTILINE)
    if not match:
        raise SystemExit("cannot read version from plugin.toml")
    return match.group(1)


def default_description():
    text = (ROOT / "plugin.toml").read_text()
    match = re.search(r'^description = "(.*)"$', text, re.MULTILINE)
    if not match:
        raise SystemExit("cannot read description from plugin.toml")
    return match.group(1)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="validate the matrix only")
    parser.add_argument("--base", help="directory with published base .yampkg files")
    parser.add_argument("--orion-runtime", type=Path, help="verified station-airplay-2.0.9-orion.yampkg for the Orion userspace fallback")
    parser.add_argument("--binary", help="ARMv7 supervisor to embed")
    parser.add_argument(
        "--extra-dir",
        default=str(ROOT),
        help="directory with extra runtime files (default: repository root)",
    )
    parser.add_argument("--version", help="output version (default: plugin.toml)")
    parser.add_argument("--out", default=str(ROOT / "dist/yampkg"))
    parser.add_argument("--matrix", default=str(ROOT / "release/yampkg/targets.json"))
    parser.add_argument(
        "--template", default=str(ROOT / "release/yampkg/launch-airplay.sh.in")
    )
    parser.add_argument("--targets", help="comma-separated subset of targets")
    parser.add_argument("--force", action="store_true", help="replace existing output")
    parser.add_argument(
        "--slim-runtime",
        action="store_true",
        help="pack the receiver and private daemons with UPX",
    )
    parser.add_argument("--upx", default="upx", help="UPX executable (default: upx)")
    args = parser.parse_args(argv)

    matrix = load_matrix(args.matrix)
    validate_matrix(matrix)
    template = Path(args.template).read_text()
    check(matrix, template)
    if args.check:
        return 0
    if not args.base or not args.binary:
        parser.error("--base and --binary are required unless --check is used")
    if not Path(args.base).is_dir():
        parser.error("--base must be a directory")

    targets = matrix["targets"]
    if args.targets:
        wanted = {item.strip() for item in args.targets.split(",") if item.strip()}
        known = {target["target"] for target in targets}
        unknown = wanted - known
        if unknown:
            parser.error("unknown targets: %s" % ", ".join(sorted(unknown)))
        targets = [target for target in targets if target["target"] in wanted]

    if any(target.get("extra_files") for target in targets):
        if not Path(args.extra_dir).is_dir():
            parser.error("--extra-dir must be a directory")
    binary = Path(args.binary).read_bytes()
    if binary[:4] != b"\x7fELF" or binary[4] != 1:
        raise SystemExit("supervisor must be ELF32")
    if int.from_bytes(binary[18:20], "little") != 40:
        raise SystemExit("supervisor must be ARM (EM_ARM=40)")
    version = args.version or default_version()
    if not re.fullmatch(r"[0-9A-Za-z][0-9A-Za-z._+-]*", version):
        parser.error("--version contains unsupported characters")
    upx = None
    if args.slim_runtime:
        try:
            upx = upx_version(args.upx)
        except FileNotFoundError:
            parser.error("--slim-runtime needs %s on PATH" % args.upx)
        print("UPX: %s" % upx)
    out_dir = Path(args.out)
    records = []
    for target in targets:
        record = build_target(
            target,
            args.base,
            binary,
            version,
            out_dir,
            args.force,
            template,
            args.extra_dir,
            slim=bool(upx),
            upx=args.upx,
            orion_runtime=args.orion_runtime,
        )
        records.append(record)
        print(
            "%-16s %9d  %s"
            % (record["target"], record["size"], record["sha256"])
        )
    provenance = out_dir / ("provenance-%s.json" % version)
    provenance.write_text(
        json.dumps(
            {
                "version": version,
                "binary_sha256": sha256_bytes(binary),
                "upx": upx,
                "packages": records,
            },
            indent=2,
        )
        + "\n"
    )
    print("built %d packages; provenance: %s" % (len(records), provenance))
    return 0


if __name__ == "__main__":
    sys.exit(main())
