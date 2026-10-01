import argparse
import json
import os
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
LOCAL = Path(__file__).resolve().parent / ".local"
POSTGRES = "postgres://postgres:poc-local-only@127.0.0.1:15439/e2b?sslmode=disable"
CLICKHOUSE = "clickhouse://clickhouse:poc-local-only@127.0.0.1:19009/default"
COMPOSE = [
    "docker",
    "compose",
    "-p",
    "e2b-insta-poc",
    "-f",
    str(ROOT / "poc/insta/compose.yaml"),
]


def execute(args, env=None, **kwargs):
    subprocess.run(args, cwd=ROOT, env=env, check=True, **kwargs)


def wait_http(url, process):
    for _ in range(120):
        if process.poll() is not None:
            raise RuntimeError("POC service exited; inspect .local logs")
        try:
            with urllib.request.urlopen(url, timeout=1) as response:
                if response.status == 200:
                    return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.5)
    raise TimeoutError("POC service did not become ready")


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=12)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--smoke", action="store_true")
    args = parser.parse_args()
    os.umask(0o077)
    LOCAL.mkdir(mode=0o700, exist_ok=True)
    required = [
        "INSTA_COMPUTE_URL",
        "INSTA_COMPUTE_TOKEN",
        "INSTA_COMPUTE_TENANT",
        "INSTA_COMPUTE_BRANCH",
        "BRIDGE_IMAGE",
    ]
    if any(not os.environ.get(key) for key in required):
        raise RuntimeError("Required environment variables: " + ", ".join(required))

    execute(COMPOSE + ["up", "-d", "--wait"])
    migrations = sorted((ROOT / "packages/db/migrations").glob("*.sql"))
    version = migrations[-1].name.split("_", 1)[0]
    execute(
        [
            "go",
            "build",
            "-ldflags",
            "-X=main.expectedMigrationTimestamp=" + version,
            "-o",
            str(LOCAL / "api"),
            "./packages/api",
        ]
    )
    execute(
        [
            "go",
            "build",
            "-o",
            str(LOCAL / "bridge"),
            "./packages/runtime-bridge/cmd/runtime-bridge",
        ]
    )
    execute(["go", "build", "-o", str(LOCAL / "seed"), "./packages/local-dev"])
    execute(
        [
            "go",
            "-C",
            "packages/db",
            "tool",
            "goose",
            "-table",
            "_migrations",
            "-dir",
            "migrations",
            "postgres",
            "up",
        ],
        env={**os.environ, "GOOSE_DBSTRING": POSTGRES},
    )
    execute(
        [
            "go",
            "-C",
            "packages/clickhouse",
            "tool",
            "goose",
            "-table",
            "_migrations",
            "-dir",
            "migrations",
            "clickhouse",
            "up",
        ],
        env={**os.environ, "GOOSE_DBSTRING": CLICKHOUSE},
    )
    execute(
        [str(LOCAL / "seed")],
        env={
            **os.environ,
            "POSTGRES_CONNECTION_STRING": POSTGRES,
            "SEED_TEAM_API_KEY": "random",
            "SEED_TEAM_API_KEY_FILE": str(LOCAL / "api-key"),
        },
    )
    with (ROOT / "poc/insta/seed-template.sql").open("rb") as source:
        execute(
            COMPOSE
            + [
                "exec",
                "-T",
                "postgres",
                "psql",
                "-v",
                "ON_ERROR_STOP=1",
                "-U",
                "postgres",
                "-d",
                "e2b",
            ],
            stdin=source,
        )

    bridge_env = {
        **os.environ,
        "BRIDGE_STATE_FILE": str(LOCAL / "state.json"),
        "BRIDGE_TEAM_ID": "0b8a3ded-4489-4722-afd1-1d82e64ec2d5",
    }
    api_env = {
        **os.environ,
        "ENVIRONMENT": "local",
        "NODE_ID": "insta-poc-api",
        "GIN_MODE": "release",
        "RUNTIME_BRIDGE_POC": "true",
        "SERVICE_DISCOVERY_PROVIDER": "local",
        "LOCAL_ORCHESTRATOR_ADDRESS": "127.0.0.1:15008",
        "POSTGRES_CONNECTION_STRING": POSTGRES,
        "REDIS_URL": "127.0.0.1:16389",
        "CLICKHOUSE_CONNECTION_STRING": CLICKHOUSE,
        "SANDBOX_ACCESS_TOKEN_HASH_SEED": (LOCAL / "api-key").read_text().strip(),
        "VOLUME_TOKEN_ENABLED": "false",
        "BEST_OF_K_HUGEPAGE_MEMORY": "false",
        "API_INTERNAL_GRPC_PORT": "15009",
        "API_EDGE_GRPC_PORT": "15109",
    }
    for key in list(api_env):
        if key.startswith("INSTA_COMPUTE_"):
            del api_env[key]
    children = []
    logs = []

    def launch(name, env, argv):
        log = (LOCAL / (name + ".log")).open("ab")
        logs.append(log)
        child = subprocess.Popen(
            argv, cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT
        )
        children.append(child)
        return child

    bridge = None
    try:
        bridge = launch("bridge", bridge_env, [str(LOCAL / "bridge")])
        wait_http("http://127.0.0.1:49983/health", bridge)
        api = launch("api", api_env, [str(LOCAL / "api"), "--port", "18080"])
        wait_http("http://127.0.0.1:18080/health", api)

        def restart_bridge():
            nonlocal bridge
            stop(bridge)
            bridge = launch("bridge", bridge_env, [str(LOCAL / "bridge")])
            wait_http("http://127.0.0.1:49983/health", bridge)

        os.environ["E2B_API_KEY"] = (LOCAL / "api-key").read_text().strip()
        os.environ["E2B_API_URL"] = "http://127.0.0.1:18080"
        os.environ["E2B_SANDBOX_URL"] = "http://127.0.0.1:49983"
        if args.smoke:
            import smoke

            evidence = smoke.run(restart_bridge)
            (LOCAL / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
        else:
            print(
                "POC ready: API http://127.0.0.1:18080; process bridge http://127.0.0.1:49983",
                flush=True,
            )
            print(
                "Use the private .local/api-key file. Ctrl-C stops local processes; kill sandboxes first.",
                flush=True,
            )
            while all(child.poll() is None for child in (api, bridge)):
                time.sleep(1)
            raise RuntimeError("POC service exited; inspect .local logs")
    except KeyboardInterrupt:
        pass
    finally:
        for child in reversed(children):
            stop(child)
        for log in logs:
            log.close()


if __name__ == "__main__":
    main()
