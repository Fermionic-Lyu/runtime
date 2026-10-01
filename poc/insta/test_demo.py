import json
import threading
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from unittest.mock import patch

import demo


class DemoHTTPTests(unittest.TestCase):
    def setUp(self):
        self.operations = []
        controller = type("Controller", (), {})()
        controller.token = "test-token"
        controller.perform = lambda action, body: (
            self.operations.append(action) or 200,
            {},
        )
        controller.lock = threading.Lock()
        self.server = ThreadingHTTPServer(
            ("127.0.0.1", 0), demo.handler_for(controller)
        )
        self.thread = threading.Thread(target=self.server.serve_forever)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def request(self, **headers):
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.server.server_port}/api/create",
            data=b"{}",
            headers={"Host": "127.0.0.1:18780", **headers},
        )
        try:
            with urllib.request.urlopen(req) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as exc:
            return exc.code, json.loads(exc.read())

    def test_cross_origin_and_untrusted_host_never_create_resources(self):
        for headers in (
            {},
            {"X-Demo-Token": "wrong"},
            {"X-Demo-Token": "test-token", "Origin": "https://evil.example"},
            {"X-Demo-Token": "test-token", "Host": "evil.example"},
        ):
            self.assertEqual(self.request(**headers)[0], 403)
        self.assertEqual(self.operations, [])
        self.assertEqual(
            self.request(**{"X-Demo-Token": "test-token", "Origin": demo.ORIGIN})[0],
            200,
        )
        self.assertEqual(self.operations, ["create"])


class DemoLifecycleTests(unittest.TestCase):
    @patch.dict(
        "os.environ", {"E2B_API_KEY": "secret", "E2B_API_URL": "http://localhost"}
    )
    def test_concurrent_create_cannot_allocate_two_sandboxes(self):
        controller = demo.Demo()
        entered, release = threading.Event(), threading.Event()

        def create(*args, **kwargs):
            entered.set()
            release.wait(5)
            raise demo.SandboxException("creation failed")

        with patch.object(demo.Sandbox, "create", side_effect=create):
            thread = threading.Thread(target=controller.perform, args=("create", {}))
            thread.start()
            try:
                self.assertTrue(entered.wait(2))
                self.assertEqual(controller.perform("create", {})[0], 409)
                self.assertTrue(controller.snapshot()["busy"])
            finally:
                release.set()
                thread.join()
        self.assertIsNone(controller.sandbox)
        self.assertFalse(controller.lock.locked())


if __name__ == "__main__":
    unittest.main()
