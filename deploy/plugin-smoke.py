#!/usr/bin/env python3
"""Validate pinned local plugins with no production credentials or external network."""
import argparse
import hashlib
import json
import os
import pathlib
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
import zipfile

MANIFEST = pathlib.Path(__file__).with_name("plugin-smoke-manifest.json")


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def extract_plugins(archives, destination):
    destination.mkdir(parents=True, exist_ok=True)
    for plugin in json.loads(MANIFEST.read_text())["plugins"]:
        target = destination / plugin["filename"]
        if target.exists():
            if sha256(target.read_bytes()) != plugin["sha256"]:
                raise RuntimeError("Existing plugin hash mismatch: " + plugin["id"])
            continue
        archive = archives / (plugin["id"] + ".zip")
        if sha256(archive.read_bytes()) != plugin["archive_sha256"]:
            raise RuntimeError("Plugin archive hash mismatch: " + plugin["id"])
        with zipfile.ZipFile(archive) as package:
            matches = [item for item in package.infolist() if item.filename.endswith(".so")]
            if len(matches) != 1 or matches[0].file_size > 64 * 1024 * 1024:
                raise RuntimeError("Unexpected plugin archive contents: " + plugin["id"])
            raw = package.read(matches[0])
        if sha256(raw) != plugin["sha256"]:
            raise RuntimeError("Plugin binary hash mismatch: " + plugin["id"])
        target.write_bytes(raw)
        print("Verified pinned plugin: " + plugin["id"], flush=True)


def block_yaml(value, indent=0):
    # The pinned priority plugin treats any leading '{' as JSON, so preserve
    # block-style mappings when the host forwards per-plugin YAML.
    lines = []
    for key, item in value.items():
        prefix = " " * indent + json.dumps(key) + ":"
        if isinstance(item, dict):
            lines.append(prefix + "\n" + block_yaml(item, indent + 2))
        else:
            lines.append(prefix + " " + json.dumps(item) + "\n")
    return "".join(lines)


def verify_isolation():
    if sys.platform != "linux":
        raise RuntimeError("Plugin smoke validation requires Linux")
    if {name for _, name in socket.if_nameindex()} != {"lo"}:
        raise RuntimeError("Run inside a network namespace containing only loopback")
    with socket.socket() as check:
        check.settimeout(0.2)
        if check.connect_ex(("1.1.1.1", 443)) == 0:
            raise RuntimeError("External network access is not isolated")


def check_candidate(binary, plugin_dir, workdir):
    verify_isolation()
    plugins = json.loads(MANIFEST.read_text())["plugins"]
    expected = {item["id"] for item in plugins}
    workdir.mkdir(parents=True, exist_ok=False)
    auth_dir = workdir / "auth"
    auth_dir.mkdir()
    local_plugins = workdir / "plugins"
    local_plugins.mkdir()
    for item in plugins:
        source = plugin_dir / item["filename"]
        if sha256(source.read_bytes()) != item["sha256"]:
            raise RuntimeError("Installed plugin hash mismatch: " + item["id"])
        shutil.copyfile(source, local_plugins / item["filename"])
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    key = secrets.token_hex(24)
    config = {
        "host": "127.0.0.1", "port": port, "auth-dir": str(auth_dir),
        "api-keys": [secrets.token_hex(24)],
        "remote-management": {"allow-remote": False, "secret-key": key,
                              "disable-control-panel": True, "disable-auto-update-panel": True},
        "plugins": {"enabled": True, "dir": str(local_plugins),
                    "configs": {item["id"]: {"enabled": True} for item in plugins}},
        "commercial-mode": True, "debug": False, "logging-to-file": False,
        "request-log": False, "usage-statistics-enabled": False,
        "antigravity": {"safety-mode": "four_off"},
        "antigravity-models": {"discovery-enabled": False},
    }
    config_file = workdir / "config.yaml"
    config_file.write_text(block_yaml(config))
    config_file.chmod(0o600)
    environment = {
        "HOME": str(workdir), "TMPDIR": str(workdir), "LANG": "C.UTF-8",
        "PATH": "/usr/bin:/bin", "GIN_MODE": "release",
        "XDG_CACHE_HOME": str(workdir / "cache"), "XDG_DATA_HOME": str(workdir / "data"),
    }
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def get(path):
        allowed = (
            "/v0/management/plugins", "/v0/management/config",
            "/v0/resource/plugins/antigravity-priority/status",
            "/v0/management/plugins/antigravity-priority/snapshot/latest",
            "/v0/management/plugins/antigravity-priority/diagnostics",
        )
        if path not in allowed:
            raise RuntimeError("Unexpected candidate probe path")
        request = urllib.request.Request("http://127.0.0.1:" + str(port) + path,
                                         headers={"Authorization": "Bearer " + key})
        with opener.open(request, timeout=5) as response:
            return response.status, response.headers, response.read()

    log_file = workdir / "candidate.log"
    result = {"network": "loopback-only", "auth_records": 0,
              "candidate_sha256": sha256(binary.read_bytes()),
              "plugin_sha256": {item["id"]: item["sha256"] for item in plugins}}
    process = None
    try:
        with log_file.open("wb") as log:
            process = subprocess.Popen([str(binary), "-config", str(config_file), "-local-model"],
                                       cwd=workdir, env=environment, stdout=log, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 60
            listing = None
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    raise RuntimeError("Candidate exited during startup")
                try:
                    _, headers, raw = get("/v0/management/plugins")
                    listing = json.loads(raw)
                    if headers.get("X-CPA-SUPPORT-PLUGIN") != "1":
                        raise RuntimeError("Candidate advertises no plugin support")
                    active = {item["id"] for item in listing["plugins"]
                              if item.get("registered") and item.get("effective_enabled")}
                    if active == expected:
                        break
                except (urllib.error.URLError, TimeoutError, ConnectionError):
                    pass
                time.sleep(0.25)
            else:
                raise RuntimeError("Plugins were not all registered and enabled: " + json.dumps(listing))
            result["support_header"] = "1"
            result["plugins"] = [{field: item[field] for field in ("id", "registered", "enabled", "effective_enabled")}
                                 for item in listing["plugins"]]
            _, _, config_raw = get("/v0/management/config")
            candidate_config = json.loads(config_raw)
            if candidate_config.get("antigravity", {}).get("safety-mode") != "four_off":
                raise RuntimeError("Candidate did not preserve safety-mode four_off")
            result["safety_mode"] = "four_off"
            result["routes"] = {}
            for path in ("/v0/resource/plugins/antigravity-priority/status",
                         "/v0/management/plugins/antigravity-priority/snapshot/latest",
                         "/v0/management/plugins/antigravity-priority/diagnostics"):
                status, _, _ = get(path)
                result["routes"][path] = status
            maps = pathlib.Path("/proc") / str(process.pid) / "maps"
            mapped = {str(local_plugins / item["filename"]) for item in plugins}
            if not all(path in maps.read_text() for path in mapped):
                raise RuntimeError("Expected plugin libraries missing from process maps")
            result["mapped_plugins"] = sorted(mapped)
            if list(auth_dir.iterdir()):
                raise RuntimeError("Smoke test unexpectedly created auth records")
            result["passed"] = True
    finally:
        # Avoid exercising the separate late-callback unload risk in this
        # credential-free, disposable process.
        if process is not None and process.poll() is None:
            os.kill(process.pid, signal.SIGKILL)
            process.wait(timeout=10)
        config_file.unlink(missing_ok=True)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--extract-archives", type=pathlib.Path)
    parser.add_argument("--binary", type=pathlib.Path)
    parser.add_argument("--plugins", type=pathlib.Path)
    parser.add_argument("--workdir", type=pathlib.Path)
    parser.add_argument("--report", type=pathlib.Path)
    args = parser.parse_args()
    if args.extract_archives:
        if not args.plugins or any((args.binary, args.workdir, args.report)):
            parser.error("--extract-archives requires only --plugins")
        extract_plugins(args.extract_archives, args.plugins)
        return
    if not all((args.binary, args.plugins, args.workdir, args.report)):
        parser.error("Candidate validation needs --binary, --plugins, --workdir and --report")
    result = check_candidate(args.binary.resolve(), args.plugins.resolve(), args.workdir.resolve())
    args.report.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
