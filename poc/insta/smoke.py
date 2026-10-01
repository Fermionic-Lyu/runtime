import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor

from e2b import Sandbox
from e2b.sandbox.commands.command_handle import CommandExitException


def request(method, url, headers, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=70) as response:
        raw = response.read()
        return response.status, json.loads(raw) if raw else None


def run(restart_bridge=None):
    api_url = os.environ.get("E2B_API_URL", "http://127.0.0.1:18080")
    options = {"api_url": api_url, "api_key": os.environ["E2B_API_KEY"]}
    headers = {"X-API-Key": options["api_key"], "Content-Type": "application/json"}
    started = time.monotonic()
    sandbox = Sandbox.create(
        "instapoc", timeout=120, envs={"POC_VALUE": "from-create"}, **options
    )
    sandbox_id = sandbox.sandbox_id
    evidence = {
        "sandbox_id": sandbox_id,
        "create_seconds": round(time.monotonic() - started, 3),
    }
    try:
        assert sandbox.get_info().sandbox_id == sandbox_id
        assert any(
            item.sandbox_id == sandbox_id
            for item in Sandbox.list(**options).next_items()
        )
        evidence["create_get_list"] = "passed"

        result = sandbox.commands.run(
            'printf "%s\\n" "$POC_VALUE"; pwd', user="root", cwd="/tmp"
        )
        assert result.stdout == "from-create\n/tmp\n" and result.exit_code == 0
        result = sandbox.commands.run(
            'printf "%s" "$QUOTED"', user="root", envs={"QUOTED": "a'\"$HOME; literal"}
        )
        assert result.stdout == "a'\"$HOME; literal"
        evidence["command_env_cwd_quoting"] = "passed"

        try:
            sandbox.commands.run("printf out; printf err >&2; exit 7", user="root")
            raise AssertionError("nonzero exit code was lost")
        except CommandExitException as exc:
            assert exc.exit_code == 7 and exc.stdout == "out" and exc.stderr == "err"
        evidence["stdout_stderr_exit_code"] = "passed"

        try:
            request("POST", api_url + "/sandboxes/" + sandbox_id + "/pause", headers)
            raise AssertionError("unsupported pause was accepted")
        except urllib.error.HTTPError as exc:
            assert exc.code == 501
        assert sandbox.commands.run("printf alive", user="root").stdout == "alive"
        evidence["unsupported_pause_preserves_instance"] = "passed"

        if restart_bridge is not None:
            restart_bridge()
            assert (
                sandbox.commands.run("printf survived-restart", user="root").stdout
                == "survived-restart"
            )
            evidence["bridge_restart"] = "passed"

        sandbox.set_timeout(5)
        with ThreadPoolExecutor(max_workers=1) as pool:
            command = pool.submit(
                sandbox.commands.run, "sleep 7; printf renewed", user="root"
            )
            time.sleep(1)
            sandbox.set_timeout(120)
            assert command.result(timeout=20).stdout == "renewed"
        evidence["timeout_update"] = "passed"
    finally:
        sandbox.kill()

    compute_url = os.environ["INSTA_COMPUTE_URL"].rstrip("/")
    tenant = urllib.parse.quote(os.environ["INSTA_COMPUTE_TENANT"], safe="")
    branch = urllib.parse.urlencode({"branch": os.environ["INSTA_COMPUTE_BRANCH"]})
    compute_headers = {"Authorization": "Bearer " + os.environ["INSTA_COMPUTE_TOKEN"]}
    service_url = (
        compute_url + "/v1/" + tenant + "/services/e2b-" + sandbox_id + "?" + branch
    )
    try:
        request("GET", service_url, compute_headers)
        raise AssertionError("compute instance survived E2B deletion")
    except urllib.error.HTTPError as exc:
        assert exc.code == 404
    evidence["delete_confirmed_by_compute"] = "passed"

    expiring = Sandbox.create("instapoc", timeout=10, **options)
    expiring_url = (
        compute_url
        + "/v1/"
        + tenant
        + "/services/e2b-"
        + expiring.sandbox_id
        + "?"
        + branch
    )
    try:
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            try:
                request("GET", expiring_url, compute_headers)
            except urllib.error.HTTPError as exc:
                if exc.code == 404:
                    evidence["expiry_confirmed_by_compute"] = "passed"
                    break
                raise
            time.sleep(0.5)
        else:
            raise AssertionError("sandbox was not removed after its deadline")
    finally:
        expiring.kill()
    print(json.dumps(evidence, indent=2), flush=True)
    return evidence


if __name__ == "__main__":
    run()
