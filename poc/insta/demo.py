import hmac
import json
import os
import secrets
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from e2b import Sandbox
from e2b.exceptions import (
    AuthenticationException,
    NotFoundException,
    SandboxException,
    TemplateException,
)
from e2b.sandbox.commands.command_handle import CommandExitException
from httpx import HTTPError

ADDRESS = ("127.0.0.1", 18780)
ORIGIN = "http://127.0.0.1:18780"


class Demo:
    def __init__(self):
        self.lock = threading.Lock()
        self.sandbox = None
        self.token = secrets.token_urlsafe(32)
        self.options = {
            "api_key": os.environ["E2B_API_KEY"],
            "api_url": os.environ["E2B_API_URL"],
            "request_timeout": 70,
        }
        self.view = {"sandbox_id": None, "expires_at": None, "status": "idle"}

    def snapshot(self):
        return {**self.view, "busy": self.lock.locked()}

    def clear(self, status):
        self.sandbox = None
        self.view = {"sandbox_id": None, "expires_at": None, "status": status}

    def refresh(self):
        if self.sandbox:
            try:
                info = self.sandbox.get_info()
                self.view = {
                    **self.view,
                    "expires_at": info.end_at.timestamp(),
                    "status": "running",
                }
            except NotFoundException:
                self.clear("expired")

    def perform(self, action, body):
        if not self.lock.acquire(blocking=False):
            return 409, {"error": "上一项操作仍在进行，请稍候。"}
        try:
            self.refresh()
            if action == "state":
                return 200, self.snapshot()
            if action == "create":
                if self.sandbox:
                    return 409, {"error": "请先销毁当前 sandbox。"}
                started = time.monotonic()
                self.sandbox = Sandbox.create(
                    "instapoc",
                    timeout=180,
                    envs={"DEMO": "e2b + insta"},
                    **self.options,
                )
                self.view = {
                    "sandbox_id": self.sandbox.sandbox_id,
                    "status": "running",
                    "expires_at": time.time() + 180,
                    "create_seconds": round(time.monotonic() - started, 2),
                }
                self.refresh()
                return 200, self.snapshot()
            if not self.sandbox:
                return 409, {"error": "请先创建一个 sandbox。"}
            if action == "exec":
                command = body.get("command")
                if (
                    not isinstance(command, str)
                    or not command.strip()
                    or len(command) > 16000
                ):
                    return 400, {"error": "请输入命令，长度上限 16,000 字符。"}
                started = time.monotonic()
                try:
                    result = self.sandbox.commands.run(
                        command, user="root", cwd="/tmp", timeout=55
                    )
                except CommandExitException as exc:
                    result = exc
                return 200, {
                    "stdout": result.stdout,
                    "stderr": result.stderr,
                    "exit_code": result.exit_code,
                    "seconds": round(time.monotonic() - started, 2),
                }
            if action == "renew":
                self.sandbox.set_timeout(180)
                self.refresh()
                return 200, self.snapshot()
            if action == "delete":
                self.sandbox.kill()
                self.clear("deleted")
                return 200, self.snapshot()
            return 404, {"error": "Unknown operation"}
        except (
            SandboxException,
            AuthenticationException,
            TemplateException,
            HTTPError,
            OSError,
            ValueError,
        ) as exc:
            message = str(exc).replace(self.options["api_key"], "[redacted]")
            return 502, {"error": message[:1500]}
        finally:
            self.lock.release()

    def close(self):
        with self.lock:
            if self.sandbox:
                self.sandbox.kill()
                self.clear("deleted")


def handler_for(demo):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def respond(self, code, data, content_type="application/json; charset=utf-8"):
            payload = data if isinstance(data, bytes) else json.dumps(data).encode()
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(payload)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("X-Frame-Options", "DENY")
            self.send_header(
                "Content-Security-Policy",
                "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; connect-src 'self'",
            )
            self.end_headers()
            self.wfile.write(payload)

        def authorized(self, api=False):
            if self.headers.get("Host") != "127.0.0.1:18780":
                return False
            if self.headers.get("Origin") not in (None, ORIGIN):
                return False
            return not api or hmac.compare_digest(
                self.headers.get("X-Demo-Token", ""), demo.token
            )

        def do_GET(self):
            if not self.authorized(api=self.path.startswith("/api/")):
                return self.respond(403, {"error": "Forbidden"})
            if self.path == "/":
                html = (
                    Path(__file__)
                    .with_name("demo.html")
                    .read_text()
                    .replace("__DEMO_TOKEN__", demo.token)
                )
                return self.respond(200, html.encode(), "text/html; charset=utf-8")
            if self.path == "/health":
                return self.respond(200, {"status": "ready"})
            if self.path == "/api/state":
                if demo.lock.locked():
                    return self.respond(200, demo.snapshot())
                code, data = demo.perform("state", {})
                data["busy"] = demo.lock.locked()
                return self.respond(code, data)
            return self.respond(404, {"error": "Not found"})

        def do_POST(self):
            if not self.authorized(api=True):
                return self.respond(403, {"error": "Forbidden"})
            if self.path not in (
                "/api/create",
                "/api/exec",
                "/api/renew",
                "/api/delete",
            ):
                return self.respond(404, {"error": "Not found"})
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= 65536:
                    return self.respond(413, {"error": "Invalid request size"})
                self.connection.settimeout(5)
                body = json.loads(self.rfile.read(length))
                if not isinstance(body, dict):
                    raise TypeError("JSON object required")
            except (TypeError, ValueError, OSError):
                return self.respond(400, {"error": "Invalid JSON request"})
            code, data = demo.perform(self.path.removeprefix("/api/"), body)
            data["busy"] = demo.lock.locked()
            return self.respond(code, data)

    return Handler


def main():
    demo = Demo()
    done = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: done.set())
    signal.signal(signal.SIGINT, lambda *_: done.set())
    server = ThreadingHTTPServer(ADDRESS, handler_for(demo))
    server.timeout = 1
    print("Demo ready: " + ORIGIN, flush=True)
    try:
        while not done.is_set():
            server.handle_request()
    finally:
        server.server_close()
        demo.close()


if __name__ == "__main__":
    main()
