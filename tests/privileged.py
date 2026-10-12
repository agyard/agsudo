#!/usr/bin/env python3
"""Production helper and installer tests in a disposable Linux container.

Requires Docker; pulls debian:bookworm-slim when missing. No host installation,
privileged container or container network. AGSUDO_TEST_IMAGE can override it
with a Debian-compatible local image. AGSUDO_TEST_PACKAGE tests an extracted
release archive instead of the source layout.
"""
import json
import os
from pathlib import Path
import subprocess
import unittest
import uuid

ROOT = Path(__file__).resolve().parents[1]


class Privileged(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.name = "agsudo-test-" + uuid.uuid4().hex[:12]
        image = os.environ.get("AGSUDO_TEST_IMAGE", "debian:bookworm-slim")

        subprocess.run(["docker", "run", "-d", "--name", cls.name, "--network", "none",
                        "--entrypoint", "/bin/sh", image, "-c", "exec sleep 600"],
                       check=True, capture_output=True)
        cls.addClassCleanup(lambda: subprocess.run(["docker", "rm", "-f", cls.name], capture_output=True, check=True))
        package = Path(os.environ.get("AGSUDO_TEST_PACKAGE", str(ROOT)))
        subprocess.run(["docker", "cp", str(package / "scripts"), cls.name + ":/scripts"], check=True)
        bindir = package / "bin" if (package / "bin").is_dir() else package
        destination = "/bin" if bindir.name == "bin" else ""
        for name in ("agsudo", "agsudo-helper"):
            subprocess.run(["docker", "cp", str(bindir / name), cls.name + ":" + destination + "/" + name], check=True)
        subprocess.run(["docker", "exec", cls.name, "sh", "-c", r"""set -eu
useradd -m -u 1000 agent
useradd -m -u 1001 other
/scripts/install.sh 1000
# Check that upgrades retain the administrator's configuration.
sed -i 's/"allowed_uids": \[1000\]/"allowed_uids": [1000, 1001]/' /etc/agsudo/config.json
/scripts/install.sh 1000
"""], check=True, capture_output=True, timeout=15)

    def call(self, *args, uid=1000, check=True, env=()):
        prefix = ["docker", "exec", "-i", "-u", str(uid), "-w", "/tmp"]
        for key, value in env:
            prefix += ["-e", f"{key}={value}"]
        p = subprocess.run([*prefix, self.name, "/usr/local/bin/agsudo", *args],
                           capture_output=True, text=True, timeout=15)
        if check:
            self.assertEqual(p.returncode, 0, p.stderr + p.stdout)
        return p

    def request(self, *args, uid=1000, options=()):
        return json.loads(self.call("request", "--json", *options, "--", *args, uid=uid).stdout)["data"]

    def test_real_elevation_and_clean_environment(self):
        req = self.request("sh", "-c", 'printf "uid="; id -u; printf "gid="; id -g; printf "env=%s,%s,%s" "$HOME" "$USER" "${AGSUDO_DEV_STATE-unset}"')
        self.assertEqual(req["owner_uid"], 1000)
        self.assertEqual(req["spec"]["target"]["uid"], 0)
        self.call("approve", req["id"], "--expect-digest", req["digest"])
        p = self.call("exec", "--request", req["id"], env=(("AGSUDO_DEV_STATE", "/tmp/attacker-db"),))
        self.assertEqual(p.stdout, "uid=0\ngid=0\nenv=/root,root,unset")
        self.assertNotEqual(self.call("exec", "--request", req["id"], check=False).returncode, 0)

    def test_different_target_drops_groups_and_uid(self):
        req = self.request("sh", "-c", 'id -u; id -g; id -G; printf "%s" "$HOME"', options=("-u", "other"))
        self.call("approve", req["id"])
        p = self.call("exec", "--request", req["id"])
        self.assertEqual(p.stdout, "1001\n1001\n1001\n/home/other")

    def test_access_is_kernel_uid_not_cli_metadata(self):
        req = self.request("id")
        p = self.call("show", req["id"], "--json", uid=1001, check=False)
        self.assertEqual(json.loads(p.stderr)["error"]["code"], "not_found")
        p = self.call("approve", req["id"], "--actor", "root", "--json", uid=1001, check=False)
        self.assertEqual(json.loads(p.stderr)["error"]["code"], "forbidden")
        p = self.call("doctor", "--json", uid=12345, check=False)
        self.assertEqual(json.loads(p.stderr)["error"]["code"], "forbidden")
        self.call("cancel", req["id"])

    def test_reviewer_can_approve_but_not_run_other_users_request(self):
        req = self.request("id", uid=1001)
        self.call("approve", req["id"])
        self.assertEqual(self.call("exec", "--request", req["id"], check=False).returncode, 127)
        p = self.call("exec", "--request", req["id"], uid=1001)
        self.assertIn("uid=0(root)", p.stdout)

    def test_root_state_files_remain_private(self):
        self.call("doctor")
        p = subprocess.run(["docker", "exec", "-u", "1000", self.name, "cat", "/var/lib/agsudo/state.db"], capture_output=True)
        self.assertNotEqual(p.returncode, 0)
        p = subprocess.run(["docker", "exec", self.name, "stat", "-c", "%a %u", "/var/lib/agsudo/state.db"], check=True, capture_output=True, text=True)
        self.assertEqual(p.stdout.strip(), "600 0")


if __name__ == "__main__":
    unittest.main(verbosity=2)
