import base64
import importlib.util
import json
import pathlib
import tempfile
import threading
import urllib.request
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("smooth", pathlib.Path(__file__).with_name("smooth-release.py"))
smooth = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smooth)


class SmoothReleaseContracts(unittest.TestCase):
    def test_real_inflight_request_survives_cutover_without_replaying(self):
        entered, release_old, switched = threading.Event(), threading.Event(), threading.Event()
        active = {"old": 0}
        calls = {"old": 0, "new": 0}
        backend = {"url": ""}
        class Old(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_GET(self):
                calls["old"] += 1
                active["old"] += 1
                entered.set()
                release_old.wait(5)
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"OLD_COMPLETE")
                active["old"] -= 1
        class New(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_GET(self):
                calls["new"] += 1
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"NEW_COMPLETE")
        class Proxy(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_GET(self):
                with urllib.request.urlopen(backend["url"]) as response:
                    body = response.read()
                self.send_response(200)
                self.end_headers()
                self.wfile.write(body)
        servers = [ThreadingHTTPServer(("127.0.0.1", 0), handler) for handler in [Old, New, Proxy]]
        threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in servers]
        for thread in threads:
            thread.start()
        old_url, new_url, proxy_url = ["http://127.0.0.1:" + str(server.server_port) for server in servers]
        backend["url"] = old_url
        received = []
        def read_old():
            with urllib.request.urlopen(proxy_url) as response:
                received.append(response.read())
        client = threading.Thread(target=read_old)
        client.start()
        try:
            self.assertTrue(entered.wait(3))
            with tempfile.TemporaryDirectory() as temporary:
                deployment = smooth.SmoothRelease("sub2api", temporary, pathlib.Path(temporary) / "state")
                deployment.probe = lambda _: None
                deployment.snapshot_routes = lambda: None
                deployment.start_bridge = lambda: None
                deployment.wait_ready = lambda: None
                def route(bridge):
                    backend["url"] = new_url if bridge else old_url
                    switched.set()
                deployment.route = route
                deployment.connections = lambda _: active["old"]
                publisher = threading.Thread(target=deployment.begin)
                publisher.start()
                self.assertTrue(switched.wait(3))
                self.assertTrue(publisher.is_alive(), "Deployment stopped waiting while the old request was active")
                with urllib.request.urlopen(proxy_url) as response:
                    self.assertEqual(response.read(), b"NEW_COMPLETE")
                release_old.set()
                publisher.join(4)
                self.assertFalse(publisher.is_alive())
                client.join(3)
                self.assertEqual(received, [b"OLD_COMPLETE"])
                self.assertEqual(calls, {"old": 1, "new": 1})
        finally:
            release_old.set()
            client.join(3)
            for server, thread in zip(servers, threads):
                server.shutdown()
                server.server_close()
                thread.join(3)

    def test_cpa_config_preserves_authentication_and_only_changes_port(self):
        raw = 'host: "127.0.0.1"\nport: 8317\nauth-dir: "/fixture/auths"\napi-keys:\n  - fixture-private\n'
        self.assertEqual(smooth.cpa_bridge_config(raw, 28317), raw.replace("port: 8317", "port: 28317"))
        with self.assertRaises(RuntimeError):
            smooth.cpa_bridge_config(raw.replace("8317", "8318"), 28317)

    def test_consumer_migration_preserves_all_fields_and_rolls_back_rejection(self):
        accounts = [{"id": 5382, "credentials": {"base_url": "http://127.0.0.1:8317", "api_key": "fixture-private", "model_mapping": {"alias": "native"}, "pool_mode_retry_count": 0}},
                    {"id": 5385, "credentials": {"base_url": "http://127.0.0.1:8317", "api_key": "fixture-second"}}]
        current = {a["id"]: dict(a["credentials"]) for a in accounts}
        def call(method, path, body=None):
            account_id = int(path.rsplit("/", 1)[1])
            if method == "PUT":
                current[account_id] = dict(body["credentials"])
                return None
            return {"credentials": current[account_id]}
        self.assertEqual(smooth.migrate_consumers(call, accounts), [5382, 5385])
        self.assertEqual(current[5382], dict(accounts[0]["credentials"], base_url="http://127.0.0.1:8316"))
        current = {a["id"]: dict(a["credentials"]) for a in accounts}
        def reject(method, path, body=None):
            if method == "PUT" and path.endswith("/5385") and body["credentials"]["base_url"].endswith("8316"):
                raise RuntimeError("Fixture management rejection")
            return call(method, path, body)
        with self.assertRaises(RuntimeError):
            smooth.migrate_consumers(reject, accounts)
        self.assertEqual(current, {a["id"]: a["credentials"] for a in accounts})

    def test_routes_are_atomic_checked_and_preserve_unrelated_cpa2_proxy(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            deployment = smooth.SmoothRelease("cpa1", root, root / "state")
            path = root / "nginx.conf"
            original = b"proxy_pass http://127.0.0.1:8317;\nproxy_pass http://127.0.0.1:8318;\n"
            path.write_bytes(original)
            deployment.state_file.write_text(json.dumps({"routes": [{"path": str(path), "mode": 0o600, "data": base64.b64encode(original).decode()}]}))
            with patch.object(smooth, "command"), patch.object(smooth.os, "chown", create=True):
                deployment.route(True)
                self.assertEqual(path.read_bytes(), original.replace(b":8317", b":28317"))
                deployment.route(False)
                self.assertEqual(path.read_bytes(), original)
                path.write_bytes(original + b"# external change\n")
                with self.assertRaises(RuntimeError):
                    deployment.route(True)
                self.assertTrue(path.read_bytes().endswith(b"# external change\n"))
            path.write_bytes(original)
            calls = []
            def fail_test(*args, **_):
                calls.append(args)
                if args == ("nginx", "-t") and len(calls) == 1:
                    raise RuntimeError("Fixture invalid Nginx config")
            with patch.object(smooth, "command", side_effect=fail_test), patch.object(smooth.os, "chown", create=True):
                with self.assertRaises(RuntimeError):
                    deployment.route(True)
                self.assertEqual(path.read_bytes(), original)
                self.assertIn(("systemctl", "reload", "nginx"), calls)

    def test_switch_waits_for_old_requests_and_failure_leaves_busy_bridge_alive(self):
        with tempfile.TemporaryDirectory() as temporary:
            deployment = smooth.SmoothRelease("sub2api", temporary, pathlib.Path(temporary) / "state")
            events = []
            deployment.probe = lambda port: events.append(("probe", port))
            deployment.snapshot_routes = lambda: events.append(("snapshot",))
            deployment.start_bridge = lambda: events.append(("start_bridge",))
            deployment.wait_ready = lambda: events.append(("ready",))
            deployment.route = lambda bridge: events.append(("route", bridge))
            deployment.drain = lambda port: events.append(("drain", port)) or True
            with patch.object(smooth, "command"):
                deployment.begin()
            self.assertLess(events.index(("ready",)), events.index(("route", True)))
            self.assertLess(events.index(("route", True)), events.index(("drain", 8081)))
            events.clear()
            deployment.drain = lambda port: events.append(("drain", port)) or False
            with patch.object(smooth, "command") as commands:
                with self.assertRaises(RuntimeError):
                    deployment.begin()
                self.assertIn(("route", False), events)
                commands.assert_not_called()

    def test_drain_requires_two_empty_snapshots(self):
        with tempfile.TemporaryDirectory() as temporary:
            deployment = smooth.SmoothRelease("sub2api", temporary, pathlib.Path(temporary) / "state")
            snapshots = iter([3, 0, 2, 0, 0])
            deployment.connections = lambda _: next(snapshots)
            with patch.object(smooth.time, "sleep"):
                self.assertTrue(deployment.drain(8081))

    def test_long_drain_reports_flushed_progress_without_stopping_requests(self):
        with tempfile.TemporaryDirectory() as temporary:
            deployment = smooth.SmoothRelease("sub2api", temporary, pathlib.Path(temporary) / "state")
            now = [0.0]
            deployment.connections = lambda _: 1 if now[0] < 35 else 0
            def advance(seconds):
                now[0] += seconds
            with patch.object(smooth.time, "monotonic", side_effect=lambda: now[0]), \
                 patch.object(smooth.time, "sleep", side_effect=advance), \
                 patch("builtins.print") as progress:
                self.assertTrue(deployment.drain(8081))
            records = [json.loads(call.args[0]) for call in progress.call_args_list]
            self.assertEqual([record["elapsed_seconds"] for record in records], [0, 15, 30])
            self.assertTrue(all(record["active_connections"] == 1 for record in records))
            self.assertTrue(all(call.kwargs.get("flush") is True for call in progress.call_args_list))
            self.assertEqual(now[0], 36.0)


if __name__ == "__main__":
    unittest.main()
