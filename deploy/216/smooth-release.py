"""Warm an immutable release and route traffic around the canonical restart."""
import argparse
import base64
import hashlib
import hmac
import json
import os
import pathlib
import re
import shlex
import subprocess
import time
import urllib.request


def command(*args, required=True):
    result = subprocess.run(args, capture_output=True, text=True)
    if required and result.returncode:
        raise RuntimeError("Deployment command failed: " + args[0])
    return result


def atomic_write(path, data, mode=None, owner=None):
    path = pathlib.Path(path)
    temporary = path.with_name(path.name + ".release-next")
    if mode is None:
        mode = path.stat().st_mode & 0o777 if path.exists() else 0o600
    temporary.write_bytes(data)
    temporary.chmod(mode)
    if owner:
        os.chown(temporary, *owner)
    elif path.exists():
        os.chown(temporary, path.stat().st_uid, path.stat().st_gid)
    os.replace(temporary, path)


def cpa_bridge_config(raw, port):
    if not re.search(r"^port:\s*8317\s*$", raw, re.M):
        raise RuntimeError("CPA1 canonical port is not 8317")
    return re.sub(r"^port:\s*8317\s*$", "port: " + str(port), raw, flags=re.M)


def migrate_consumers(call, accounts):
    """Update only verified CPA1 URLs, keeping every credential field intact."""
    changed = []
    try:
        for account in accounts:
            original = account["credentials"]
            if original.get("base_url") != "http://127.0.0.1:8317":
                continue
            updated = dict(original, base_url="http://127.0.0.1:8316")
            call("PUT", "/accounts/" + str(account["id"]), {"credentials": updated})
            changed.append(account)
            saved = call("GET", "/accounts/" + str(account["id"]))
            if saved["credentials"] != updated:
                raise RuntimeError("CPA1 ingress account readback differs")
    except Exception:
        for account in reversed(changed):
            call("PUT", "/accounts/" + str(account["id"]), {"credentials": account["credentials"]})
        raise
    return [account["id"] for account in changed]


def prepare_cpa_consumers(state_dir):
    cfg = pathlib.Path("/opt/sub2api/config.yaml").read_text()
    block = re.search(r"(?ms)^database:\s*\n(.*?)(?=^\S|\Z)", cfg).group(1)
    env = os.environ.copy()
    env["PGPASSWORD"] = re.search(r"^\s+password:\s*(.*?)\s*$", block, re.M).group(1).strip("\"'")
    env["PGOPTIONS"] = "-c default_transaction_read_only=on -c statement_timeout=10000"

    def sql(query):
        result = subprocess.run(["psql", "-X", "-h", "127.0.0.1", "-U", "quizcast", "-d", "sub2api", "-t", "-A", "-v", "ON_ERROR_STOP=1", "-c", query], env=env, capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError("CPA1 consumer read failed")
        return json.loads(result.stdout)

    admin = sql("SELECT row_to_json(t) FROM (SELECT id,email,password_hash FROM users WHERE role='admin' ORDER BY id LIMIT 1)t")
    secret = re.search(r"(?ms)^jwt:\s*\n(.*?)(?=^\S|\Z)", cfg).group(1)
    secret = re.search(r"^\s+secret:\s*(.*?)\s*$", secret, re.M).group(1).strip("\"'")
    fingerprint = int.from_bytes(hashlib.sha256((admin["email"].strip().lower() + "\n" + admin["password_hash"]).encode()).digest()[:8], "big") & 0x7FFFFFFFFFFFFFFF
    b64 = lambda data: base64.urlsafe_b64encode(data).rstrip(b"=").decode()
    head = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
    payload = b64(json.dumps({"user_id": admin["id"], "email": admin["email"], "role": "admin", "token_version": fingerprint, "iat": int(time.time()), "nbf": int(time.time()) - 1, "exp": int(time.time()) + 3600}).encode())
    token = head + "." + payload + "." + b64(hmac.new(secret.encode(), (head + "." + payload).encode(), hashlib.sha256).digest())

    def call(method, path, body=None):
        request = urllib.request.Request("http://127.0.0.1:8081/api/v1/admin" + path, method=method, data=json.dumps(body).encode() if body is not None else None, headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=20) as response:
            return json.load(response)["data"]

    ids = sql("SELECT COALESCE(json_agg(id),'[]'::json) FROM accounts WHERE deleted_at IS NULL AND credentials->>'base_url'='http://127.0.0.1:8317'")
    if set(ids) - {5382, 5385, 5386}:
        raise RuntimeError("Unexpected CPA1 local consumer; refusing an unreviewed account update")
    accounts = [call("GET", "/accounts/" + str(account_id)) for account_id in ids]
    expected_platforms = {5382: "gemini", 5385: "anthropic", 5386: "openai"}
    if any(account.get("platform") != expected_platforms[account["id"]] or account.get("type") != "apikey" for account in accounts):
        raise RuntimeError("CPA1 consumer identity differs from the reviewed production binding")
    atomic_write(state_dir / "cpa1-consumers-before.json", json.dumps(accounts).encode(), 0o600)
    changed = migrate_consumers(call, accounts)
    print(json.dumps({"cpa1_stable_ingress_accounts": changed}), flush=True)


class SmoothRelease:
    def __init__(self, component, release, state_dir):
        self.component = component
        self.release = pathlib.Path(release).resolve()
        self.state_dir = pathlib.Path(state_dir).resolve()
        self.state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.state_file = self.state_dir / "smooth-state.json"
        self.unit = "sub2api" if component == "sub2api" else "cliproxyapi"
        self.bridge = self.unit + "-release-bridge"
        self.port = 8081 if component == "sub2api" else 8317
        self.bridge_port = 18081 if component == "sub2api" else 28317
        self.health = "/health" if component == "sub2api" else "/healthz"
        self.drain_seconds = int(os.environ.get("RELEASE_DRAIN_SECONDS", "600"))

    def probe(self, port):
        with urllib.request.urlopen("http://127.0.0.1:" + str(port) + self.health, timeout=3) as response:
            if response.status != 200:
                raise RuntimeError("Release HTTP readiness failed")

    def wait_ready(self):
        for _ in range(60):
            try:
                command("systemctl", "is-active", "--quiet", self.bridge)
                self.probe(self.bridge_port)
                if command("systemctl", "show", self.bridge, "--property=NRestarts", "--value").stdout.strip() != "0":
                    raise RuntimeError("Release bridge restarted")
                pid = command("systemctl", "show", self.bridge, "--property=MainPID", "--value").stdout.strip()
                binary = self.release / ("sub2api" if self.component == "sub2api" else "cli-proxy-api")
                if not pid.isdecimal() or pid == "0" or hashlib.sha256(pathlib.Path("/proc/" + pid + "/exe").read_bytes()).digest() != hashlib.sha256(binary.read_bytes()).digest():
                    raise RuntimeError("Release bridge process differs from the reviewed artifact")
                return
            except Exception:
                time.sleep(1)
        raise RuntimeError("Release bridge failed readiness")

    def connections(self, port):
        result = command("ss", "-Htn", "state", "established", "sport", "=", ":" + str(port))
        return len(result.stdout.splitlines())

    def drain(self, port):
        started = time.monotonic()
        deadline = started + self.drain_seconds
        next_progress = started
        # Two empty snapshots avoid stopping a process while a reload is settling.
        empty = 0
        while time.monotonic() < deadline:
            active = self.connections(port)
            now = time.monotonic()
            if now >= next_progress:
                print(json.dumps({"component": self.component, "phase": "drain", "port": port,
                                  "active_connections": active, "elapsed_seconds": int(now - started),
                                  "remaining_seconds": max(0, int(deadline - now))}), flush=True)
                next_progress = now + 15
            empty = empty + 1 if active == 0 else 0
            if empty >= 2:
                return True
            time.sleep(1)
        return False

    def stable_cpa_ingress(self):
        path = pathlib.Path("/etc/nginx/conf.d/cpa1-local-ingress.conf")
        text = """# Managed CPA1 release ingress; never routes to CPA2.
server {
    listen 127.0.0.1:8316;
    server_name _;
    client_max_body_size 100m;
    location / {
        proxy_pass http://127.0.0.1:8317;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_buffering off;
        proxy_read_timeout 86400s;
        proxy_send_timeout 86400s;
    }
}
"""
        if path.exists() and path.read_text() != text:
            raise RuntimeError("CPA1 stable ingress contains unreviewed changes")
        if not path.exists():
            atomic_write(path, text.encode(), 0o644)
            try:
                command("nginx", "-t")
                command("systemctl", "reload", "nginx")
                self.probe(8316)
            except Exception:
                path.unlink()
                command("nginx", "-t")
                command("systemctl", "reload", "nginx")
                raise
        prepare_cpa_consumers(self.state_dir)

    def snapshot_routes(self):
        if self.component == "sub2api":
            paths = ["/etc/nginx/conf.d/nodus-sub2api-migration.conf"]
        else:
            paths = ["/etc/nginx/conf.d/cpa1-local-ingress.conf", "/etc/nginx/conf.d/cpa.conf", "/etc/nginx/conf.d/cpa-admin.conf"]
        saved = []
        old = "127.0.0.1:" + str(self.port)
        for name in paths:
            path = pathlib.Path(name)
            raw = path.read_bytes()
            if old.encode() not in raw:
                raise RuntimeError("Expected canonical route is missing")
            saved.append({"path": name, "data": base64.b64encode(raw).decode(), "mode": path.stat().st_mode & 0o777})
        atomic_write(self.state_file, json.dumps({"routes": saved}).encode(), 0o600)

    def route(self, bridge):
        state = json.loads(self.state_file.read_text())
        old = ("127.0.0.1:" + str(self.port)).encode()
        new = ("127.0.0.1:" + str(self.bridge_port)).encode()
        before = [(pathlib.Path(item["path"]), pathlib.Path(item["path"]).read_bytes()) for item in state["routes"]]
        for item, (_, current) in zip(state["routes"], before):
            original = base64.b64decode(item["data"])
            if current not in [original, original.replace(old, new)]:
                raise RuntimeError("Ingress changed during deployment; refusing to overwrite it")
        try:
            for item in state["routes"]:
                raw = base64.b64decode(item["data"])
                atomic_write(item["path"], raw.replace(old, new) if bridge else raw, item["mode"])
            command("nginx", "-t")
            command("systemctl", "reload", "nginx")
        except Exception:
            for path, raw in before:
                atomic_write(path, raw)
            command("nginx", "-t")
            command("systemctl", "reload", "nginx")
            raise
        print(json.dumps({"component": self.component, "route": "bridge" if bridge else "canonical", "port": self.bridge_port if bridge else self.port}), flush=True)

    def start_bridge(self):
        if command("systemctl", "is-active", "--quiet", self.bridge, required=False).returncode == 0:
            raise RuntimeError("An earlier release bridge is still active")
        if command("ss", "-Hltn", "sport", "=", ":" + str(self.bridge_port)).stdout.strip():
            raise RuntimeError("Release bridge port is already occupied")
        command("systemctl", "reset-failed", self.bridge, required=False)
        values = {}
        for line in command("systemctl", "show", self.unit, "--property=User,Group,WorkingDirectory,Environment,EnvironmentFiles").stdout.splitlines():
            if "=" in line:
                key, value = line.split("=", 1)
                values[key] = value
        user = values.get("User") or "root"
        group = values.get("Group") or user
        working = pathlib.Path(values["WorkingDirectory"])
        properties = ["--property=User=" + user, "--property=Group=" + group, "--property=WorkingDirectory=" + str(working)]
        env_path = self.state_dir / "bridge.env"
        env_lines = list(shlex.split(values.get("Environment", "")))
        for entry in re.findall(r"(\S+)\s+\(ignore_errors=\S+\)", values.get("EnvironmentFiles", "")):
            if pathlib.Path(entry).exists():
                env_lines += pathlib.Path(entry).read_text().splitlines()
        if self.component == "sub2api":
            env_lines += ["SERVER_HOST=127.0.0.1", "SERVER_PORT=" + str(self.bridge_port)]
        atomic_write(env_path, ("\n".join(env_lines) + "\n").encode(), 0o600)
        properties.append("--property=EnvironmentFile=" + str(env_path))
        if self.component == "sub2api":
            binary = self.release / "sub2api"
            args = [str(binary)]
        else:
            binary = self.release / "cli-proxy-api"
            config = self.release / "bridge-config.yaml"
            import pwd
            identity = pwd.getpwnam(user)
            # Permit only the service owner to read its protected bridge config.
            atomic_write(config, cpa_bridge_config(pathlib.Path("/etc/cliproxyapi/config.yaml").read_text(), self.bridge_port).encode(), 0o600, (identity.pw_uid, identity.pw_gid))
            args = [str(binary), "-config", str(config), "-local-model"]
        command("systemd-run", "--unit=" + self.bridge, "--collect", "--quiet", "--property=Restart=no", "--property=KillMode=mixed", "--property=TimeoutStopSec=90", *properties, "--", *args)

    def begin(self):
        self.probe(self.port)
        if self.component == "cpa1":
            self.stable_cpa_ingress()
        self.snapshot_routes()
        self.start_bridge()
        try:
            self.wait_ready()
            self.route(True)
            if not self.drain(self.port):
                raise RuntimeError("Existing connections did not drain; canonical service was not stopped")
        except Exception:
            self.route(False)
            if self.drain(self.bridge_port):
                command("systemctl", "stop", self.bridge)
            # Keep a busy bridge alive for its in-flight requests; fail CI.
            raise
        print(json.dumps({"component": self.component, "old_connections": 0, "bridge_ready": True}), flush=True)

    def finish(self):
        self.probe(self.port)
        self.route(False)
        if not self.drain(self.bridge_port):
            raise RuntimeError("Bridge still has active requests; leave it alive for draining")
        command("systemctl", "stop", self.bridge)
        print(json.dumps({"component": self.component, "bridge_drained": True}), flush=True)

    def hold(self):
        # Protect traffic before a rollback restarts the canonical process.
        self.probe(self.bridge_port)
        self.route(True)
        if not self.drain(self.port):
            raise RuntimeError("Canonical connections did not drain before rollback")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("component", choices=["sub2api", "cpa1"])
    parser.add_argument("action", choices=["begin", "finish", "hold"])
    parser.add_argument("release")
    parser.add_argument("state_dir")
    args = parser.parse_args()
    deployment = SmoothRelease(args.component, args.release, args.state_dir)
    try:
        getattr(deployment, args.action)()
    except Exception as error:
        raise SystemExit("Smooth release failed: " + str(error)) from None
