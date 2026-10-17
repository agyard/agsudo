#!/usr/bin/env python3
"""End-to-end CLI tests, including real terminals, without host root access."""
import fcntl
import json
import os
from pathlib import Path
import pty
import pwd
import select
import signal
import struct
import subprocess
import tempfile
import termios
import time
import unittest

ROOT = Path(__file__).resolve().parents[1]
CLI = str(ROOT / "bin/agsudo")


class Terminal:
    def __init__(self, args, env, width=110, height=32):
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.execve(CLI, [CLI, *args], env)
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))
        self.output = b""
        self.status = None

    def send(self, text):
        os.write(self.fd, text.encode())

    def until(self, text, timeout=10):
        target = text.encode()
        deadline = time.monotonic() + timeout
        while target not in self.output:
            if time.monotonic() >= deadline:
                raise AssertionError(f"terminal missing {text!r}: {self.output[-6000:]!r}")
            if select.select([self.fd], [], [], 0.1)[0]:
                try:
                    data = os.read(self.fd, 65536)
                except OSError:
                    data = b""
                if not data:
                    raise AssertionError(f"terminal closed before {text!r}: {self.output[-6000:]!r}")
                self.output += data
        return self.output

    def wait(self, timeout=10):
        deadline = time.monotonic() + timeout
        while self.status is None:
            pid, status = os.waitpid(self.pid, os.WNOHANG)
            if pid:
                self.status = os.waitstatus_to_exitcode(status)
                return self.status
            if time.monotonic() >= deadline:
                raise AssertionError("terminal process did not exit")
            if select.select([self.fd], [], [], 0.1)[0]:
                try:
                    self.output += os.read(self.fd, 65536)
                except OSError:
                    pass
        return self.status

    def close(self):
        if self.status is None:
            try:
                os.kill(self.pid, signal.SIGKILL)
                os.waitpid(self.pid, 0)
            except ProcessLookupError:
                pass
        os.close(self.fd)


class Integration(unittest.TestCase):
    def setUp(self):
        if os.getuid() == 0:
            self.skipTest("development helper intentionally refuses root; use privileged.py")
        self.temp = tempfile.TemporaryDirectory(prefix="agsudo-test-")
        self.addCleanup(self.temp.cleanup)
        self.env = dict(os.environ, AGSUDO_HELPER=str(ROOT / "bin/agsudo-helper-dev"),
                        AGSUDO_DEV_STATE=self.temp.name + "/state",
                        XDG_CACHE_HOME=self.temp.name + "/cache", TERM="xterm-256color")
        self.env.pop("AGSUDO_SESSION", None)
        self.env.pop("AGSUDO_REASON", None)

    def call(self, *args, check=True, input=None):
        p = subprocess.run([CLI, *args], env=self.env, text=True, input=input,
                           capture_output=True, timeout=15)
        if check:
            self.assertEqual(p.returncode, 0, p.stderr + p.stdout)
        return p

    def data(self, *args):
        return json.loads(self.call(*args, "--json").stdout)["data"]

    def request(self, *command, options=()):
        return json.loads(self.call("request", "--json", *options, "--", *command).stdout)["data"]

    def approve(self, req):
        return self.data("approve", req["id"], "--expect-digest", req["digest"], "--actor", "agent:test")

    def test_command_streams_exit_status_and_one_shot(self):
        req = self.request("sh", "-c", "printf out; printf err >&2; exit 7")
        self.approve(req)
        p = self.call("exec", "--request", req["id"], check=False)
        self.assertEqual((p.returncode, p.stdout, p.stderr), (7, "out", "err"))
        self.assertEqual(self.data("status", req["id"])["exit_code"], 7)
        self.assertNotEqual(self.call("exec", "--request", req["id"], check=False).returncode, 0)
        events = [json.loads(x) for x in self.call("audit", "export").stdout.splitlines()]
        self.assertEqual([e["type"] for e in events], ["request.created", "request.approved", "execution.claimed", "execution.started", "execution.exited"])
        last = events[-2]["sequence"]
        tail = self.call("audit", "export", "--after", str(last)).stdout.splitlines()
        self.assertEqual(len(tail), 1)

    def test_sudo_compatibility_environment(self):
        req = self.request("sh", "-c", 'printf "%s|%s|%s" "$SUDO_USER" "$SUDO_COMMAND" "$SUDO_UID"')
        self.approve(req)
        p = self.call("exec", "--request", req["id"])
        command = " ".join(req["spec"]["argv"])
        self.assertEqual(p.stdout, f"{pwd.getpwuid(os.getuid()).pw_name}|{command}|{os.getuid()}")

    def test_command_umask_follows_sudo(self):
        for caller, child in (("000", "0022"), ("027", "0027")):
            req = self.request("sh", "-c", "umask")
            self.approve(req)
            p = subprocess.run(["sh", "-c", f'umask {caller}; exec "$0" exec --request "$1"', CLI, req["id"]],
                               env=self.env, text=True, capture_output=True, timeout=15)
            self.assertEqual(p.stdout.strip(), child, p.stderr)

    def test_terminal_interrupt_reaches_command_once(self):
        req = self.request("python3", "-c", "import signal, time\nn = []\nsignal.signal(signal.SIGINT, lambda *_: n.append(1))\nprint('READY', flush=True)\ntime.sleep(1.5)\nprint('COUNT', len(n))")
        self.approve(req)
        tty = Terminal(["exec", "--request", req["id"]], self.env)
        self.addCleanup(tty.close)
        tty.until("READY")
        tty.send("\x03")
        tty.until("COUNT")
        self.assertEqual(tty.wait(), 0)
        self.assertIn(b"COUNT 1", tty.output)

    def test_cancel_expiry_and_defer(self):
        req = self.request("id")
        self.data("defer", req["id"], "--recommend", "approve", "--summary", "needs a person")
        self.assertEqual(self.data("review")["requests"], [])
        self.assertEqual(len(self.data("review", "--state", "needs_human")["requests"]), 1)
        self.call("cancel", req["id"])
        self.assertNotEqual(self.call("approve", req["id"], check=False).returncode, 0)
        req = self.request("id", options=("--ttl", "1s"))
        self.approve(req)
        time.sleep(1.05)
        self.assertEqual(self.data("status", req["id"])["state"], "expired")
        p = self.call("exec", "--request", req["id"], check=False)
        self.assertEqual((p.returncode, p.stderr), (1, f"agsudo: {req['id']} expired; command was not started\n"))

    def test_context_and_changed_executable(self):
        script = Path(self.temp.name) / "script"
        script.write_text("#!/bin/sh\nprintf original\n")
        script.chmod(0o700)
        context = Path(self.temp.name) / "notes.txt"
        context.write_text("Install build dependencies \u2014 within the approved scope\n")
        req = self.request(str(script), options=("--context", str(context)))
        self.assertEqual(req["spec"]["context"][0]["content"], context.read_text())
        self.approve(req)
        script.write_text("#!/bin/sh\nprintf changed\n")
        p = self.call("exec", "--request", req["id"], check=False)
        self.assertEqual((p.returncode, p.stdout), (1, ""))
        self.assertIn("executable changed", p.stderr)
        self.assertEqual(self.data("status", req["id"])["state"], "failed")

    def test_shebang_and_piped_stdin(self):
        script = Path(self.temp.name) / "script"
        script.write_text('#!/bin/sh\ncat\nprintf "arg:%s" "$1"\ncat "$(dirname "$0")/adjacent"\n')
        script.chmod(0o700)
        (script.parent / "adjacent").write_text(" adjacent")
        req = self.request(str(script), "--not-an-agsudo-option")
        self.approve(req)
        p = self.call("exec", "--request", req["id"], input="input\n")
        self.assertEqual(p.stdout, "input\narg:--not-an-agsudo-option adjacent")

    def process(self, *args, executable=CLI):
        # A new session lets tests signal the whole process group, as agent hosts do.
        p = subprocess.Popen([executable, *args], env=self.env, stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
        def cleanup():
            if p.poll() is None:
                p.send_signal(signal.SIGINT)
                try:
                    p.communicate(timeout=3)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.communicate(timeout=3)
            p.stdout.close()
            p.stderr.close()
        self.addCleanup(cleanup)
        return p

    def pending(self, process=None):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            # Surface child startup errors instead of reporting a queue timeout.
            if process is not None and process.poll() is not None:
                stdout, stderr = process.communicate(timeout=3)
                self.fail(f"process exited with {process.returncode}: {stdout}{stderr}")
            requests = self.data("review")["requests"]
            if requests:
                return requests[-1]
            time.sleep(0.02)
        self.fail("request was not submitted")

    def start_waiting(self, *args):
        p = self.process(*args)
        return p, self.pending(p)["id"]

    def test_ctrl_c_cancels_but_timeouts_resume(self):
        p, rid = self.start_waiting("exec", "--", "true")
        p.send_signal(signal.SIGINT)
        self.assertEqual(p.communicate(timeout=10)[1], f"agsudo: canceled {rid}; command was not started\n")
        self.assertEqual(self.data("status", rid)["state"], "canceled")
        self.assertNotEqual(self.call("approve", rid, check=False).returncode, 0)
        target = Path(self.temp.name) / "out.txt"
        piped = f"printf data | sudo tee {target}"
        for sig in (signal.SIGTERM, signal.SIGKILL):
            p, rid = self.start_waiting("wrap", "--session", "host", "--", "sh", "-c", piped)
            os.killpg(p.pid, sig)
            _, stderr = p.communicate(timeout=10)
            if sig == signal.SIGTERM:
                self.assertIn(f"{rid} stays pending", stderr)
            self.approve(self.data("status", rid))
            # Running the same command again reuses the request, pipe included.
            again = self.call("wrap", "--session", "host", "--", "sh", "-c", piped)
            self.assertEqual((again.stdout, target.read_text()), ("data", "data"))
            self.assertEqual(self.data("status", rid)["state"], "exited")

    def test_wrap_intercepts_sudo_and_preserves_args(self):
        p, rid = self.start_waiting("wrap", "--", "sh", "-c", "sudo printf '%s' --json")
        req = self.data("show", rid)
        self.assertTrue(req["spec"]["session"].startswith("ses_"))
        self.assertEqual(req["spec"]["argv"], ["printf", "%s", "--json"])
        self.approve(req)
        stdout, stderr = p.communicate(timeout=10)
        self.assertEqual((p.returncode, stdout), (0, "--json"), stderr)

    def test_reviewer_guardrails(self):
        req = self.request("true", options=("--session", "mine"))
        p = self.call("--json", "approve", req["id"], check=False)
        self.assertEqual(p.returncode, 2)
        self.env["AGSUDO_SESSION"] = "mine"
        p = self.call("--json", "approve", req["id"], "--expect-digest", req["digest"], check=False)
        self.assertEqual(json.loads(p.stderr)["error"]["code"], "forbidden")
        del self.env["AGSUDO_SESSION"]
        self.data("defer", req["id"])
        p = self.call("--json", "approve", req["id"], "--expect-digest", req["digest"], check=False)
        self.assertIn("deferred to a person", json.loads(p.stderr)["error"]["message"])
        self.assertEqual(self.call("approve", req["id"]).stdout, f"{req['id']} approved\n")

    def test_noninteractive_and_json_usage(self):
        p = self.call("wrap", "--", "sudo", "-n", "true", check=False)
        self.assertEqual(p.returncode, 1)
        self.assertEqual(self.data("review", "--all")["requests"], [])
        p = self.call("approve", "--json", check=False)
        self.assertEqual(p.returncode, 2)
        self.assertEqual(json.loads(p.stderr)["error"]["code"], "usage")
        p = self.call("review", "--tui", check=False)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("interactive terminal", p.stderr)

    def test_wrap_sudo_directly(self):
        p, rid = self.start_waiting("wrap", "--", "sudo", "printf", "%s", "wrapped")
        self.approve(self.data("show", rid))
        stdout, stderr = p.communicate(timeout=10)
        self.assertEqual((p.returncode, stdout, stderr), (0, "wrapped", ""))

    def test_sudo_help_discovers_async_workflow_and_shim(self):
        p = self.call("wrap", "--", "sh", "-c", "sudo --help")
        self.assertIn("agsudo exec --request", p.stdout)
        p = self.call("wrap", "--", "sh", "-c", "agsudo help --requester")
        self.assertIn("agsudo request", p.stdout)

    def test_wait_hint_once_and_no_hint_during_execution(self):
        p, rid = self.start_waiting("wrap", "--", "sudo", "true")
        self.assertTrue(select.select([p.stderr], [], [], 5)[0])
        hint = p.stderr.readline()
        self.assertEqual(hint, f"sudo: waiting for approval of {rid}; see 'sudo --help'\n")
        # No periodic notices; approval should start the original command.
        self.assertFalse(select.select([p.stderr], [], [], 2.2)[0])
        self.approve(self.data("status", rid))
        stdout, stderr = p.communicate(timeout=10)
        self.assertEqual((p.returncode, stdout, stderr), (0, "", ""))
        req = self.request("sh", "-c", "sleep 2.2; exit 127")
        self.approve(req)
        result = self.call("exec", "--request", req["id"], check=False)
        self.assertEqual((result.returncode, result.stderr), (127, ""))

    def test_host_hooks_route_scripts_without_wrap(self):
        for host in ("codex", "claude"):
            with self.subTest(host=host):
                self.env["CODEX_HOME"] = self.temp.name + "/codex"
                self.env["CLAUDE_CONFIG_DIR"] = self.temp.name + "/claude"
                self.call("integration", "install", host)
                config = Path(self.env["CODEX_HOME"] if host == "codex" else self.env["CLAUDE_CONFIG_DIR"])
                settings = json.loads((config / ("hooks.json" if host == "codex" else "settings.json")).read_text())
                hook_command = settings["hooks"]["PreToolUse" if host == "codex" else "SessionStart"][0]["hooks"][0]["command"]
                script = Path(self.temp.name) / "uses-sudo.sh"
                script.write_text("sudo printf '%s' 'quote: \"x\"'\n")
                original = "sh " + str(script)
                payload = {"session_id": host + "-session", "hook_event_name": "PreToolUse" if host == "codex" else "SessionStart",
                           "permission_mode": "bypassPermissions", "tool_name": "Bash",
                           "tool_input": {"command": original, "timeout_ms": 12345}}
                env_file = Path(self.temp.name) / "env.sh"
                self.env["CLAUDE_ENV_FILE"] = str(env_file)
                result = subprocess.run(["sh", "-c", hook_command], input=json.dumps(payload), env=self.env,
                                        text=True, capture_output=True, check=True, timeout=10)
                if host == "codex":
                    updated = json.loads(result.stdout)["hookSpecificOutput"]["updatedInput"]
                    self.assertEqual(updated["timeout_ms"], 12345)
                    self.assertTrue(updated["command"].endswith(original))
                    command = updated["command"]
                else:
                    self.assertIn("sudo --help", result.stdout)
                    command = env_file.read_text() + original
                # Execute what the host receives; the script itself contains
                # sudo, so searching/replacing the top-level text would miss it.
                p = self.process("-c", "PATH=/usr/bin:/bin\n" + command, executable="/bin/sh")
                req = self.pending(p)
                self.assertEqual(req["spec"]["session"], host + "-session")
                self.approve(req)
                stdout, stderr = p.communicate(timeout=10)
                self.assertEqual((p.returncode, stdout, stderr), (0, 'quote: "x"', ""))
                self.call("integration", "uninstall", host)

    def test_live_event_stream_and_resume(self):
        p = self.process("watch", "--json")
        req = self.request("true")
        self.assertTrue(select.select([p.stdout], [], [], 10)[0])
        event = json.loads(p.stdout.readline())
        self.assertEqual((event["type"], event["request_id"]), ("request.created", req["id"]))
        p.send_signal(signal.SIGINT)
        p.communicate(timeout=10)
        self.approve(req)
        p = self.process("watch", "--json", "--after", str(event["sequence"]))
        self.assertTrue(select.select([p.stdout], [], [], 10)[0])
        event = json.loads(p.stdout.readline())
        self.assertEqual(event["type"], "request.approved")
        p.send_signal(signal.SIGINT)
        p.communicate(timeout=10)

    def test_tui_can_approve_and_quit(self):
        req = self.request("id", options=("--reason", "TUI approval test"))
        tty = Terminal(["review", "--tui"], self.env)
        self.addCleanup(tty.close)
        tty.until("TUI approval test")
        tty.send("a")
        tty.until("APPROVE id")
        tty.send("looks good\r")
        deadline = time.monotonic() + 10
        while self.data("status", req["id"])["state"] != "approved":
            self.assertLess(time.monotonic(), deadline)
            time.sleep(0.1)
        tty.send("q")
        self.assertEqual(tty.wait(), 0)

    def test_interactive_shell_ctrl_c_and_job_control(self):
        req = self.request("bash")
        self.approve(req)
        tty = Terminal(["exec", "--request", req["id"]], self.env)
        self.addCleanup(tty.close)
        tty.send("printf 'AGREADY:%s\\n' \"$(id -u)\"\r")
        tty.until(f"AGREADY:{os.getuid()}\r\n")
        tty.send("sleep 20\r")
        time.sleep(0.2)
        tty.send("\x1a")
        tty.until("Stopped")
        tty.send("fg\r")
        time.sleep(0.2)
        tty.send("\x03")
        tty.send("printf 'RESUMED:%s\\n' ok\r")
        tty.until("RESUMED:ok\r\n")
        tty.send("stty size\r")
        tty.until("32 110\r\n")
        tty.send("exit 17\r")
        self.assertEqual(tty.wait(), 17)
        self.assertEqual(self.data("status", req["id"])["exit_code"], 17)


if __name__ == "__main__":
    unittest.main(verbosity=2)
