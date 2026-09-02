"""Regression tests for adapters/check-no-api-error-traj.sh.

The gate exists because a trial whose claude CLI retried through
429s/overloads/connection drops can still "succeed" -- harbor's result.json
only surfaces TERMINAL API errors. What ships from such a run is a poisoned
or truncated transcript. These tests pin the event-level detection and,
just as important, the false-positive class the naive grep had: tool_result
content (target-program logs, source code) legitimately contains "429" and
"connection reset" -- that must pass (2026-08-31 sweep: 11 trials matched
naive greps, 0 were real).
"""
import json
import os
import subprocess
import tempfile
import unittest

ADAPTER = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check-no-api-error-traj.sh")


def run_gate(traj_dir):
    """Run the adapter against a directory; return (exit_code, stdout, stderr)."""
    env = dict(os.environ, TRAJ_DIR=traj_dir)
    p = subprocess.run(["bash", ADAPTER], env=env, capture_output=True, text=True, timeout=60)
    return p.returncode, p.stdout, p.stderr


def ev(**kw):
    return json.dumps(kw)


class ApiErrorGateTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()

    def _write(self, name, lines):
        path = os.path.join(self.dir, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")

    INIT = ev(type="system", subtype="init", session_id="s", model="kimi-k3")
    ASSISTANT = ev(type="assistant", message={"role": "assistant", "content": [
        {"type": "text", "text": "reading the source now"}]})
    RESULT_OK = ev(type="result", subtype="success", num_turns=42)

    def test_clean_transcript_passes(self):
        self._write("agent/claude-code.txt", [self.INIT, self.ASSISTANT, self.RESULT_OK])
        rc, out, _ = run_gate(self.dir)
        self.assertEqual(rc, 0, out)

    def test_is_api_error_message_blocks(self):
        self._write("agent/claude-code.txt", [self.INIT, ev(
            type="assistant",
            message={"role": "assistant", "isApiErrorMessage": True,
                     "content": [{"type": "text", "text": "API Error: 429 ..."}]}),
            self.RESULT_OK])
        rc, _, err = run_gate(self.dir)
        self.assertEqual(rc, 1, "isApiErrorMessage must block")
        self.assertIn("isApiErrorMessage", err)

    def test_system_api_error_blocks(self):
        self._write("agent/claude-code.txt", [self.INIT, ev(
            type="system", subtype="api_error", error={"status": 429}), self.RESULT_OK])
        rc, _, err = run_gate(self.dir)
        self.assertEqual(rc, 1, "system/api_error must block")

    def test_error_result_subtype_blocks(self):
        self._write("agent/claude-code.txt", [self.INIT, self.ASSISTANT,
                                              ev(type="result", subtype="error_during_execution")])
        rc, _, err = run_gate(self.dir)
        self.assertEqual(rc, 1, "non-success result must block")
        self.assertIn("error_during_execution", err)

    def test_truncated_stream_blocks(self):
        # Events but no result event: the CLI died mid-run (kill, timeout,
        # network death). Exactly the shape of the 9 kill-leftover trials
        # found in the 2026-08-31 sweep.
        self._write("agent/claude-code.txt", [self.INIT, self.ASSISTANT])
        rc, _, err = run_gate(self.dir)
        self.assertEqual(rc, 1, "truncated stream must block")
        self.assertIn("truncated", err)

    def test_tool_result_429_passes(self):
        # The false-positive class: 429s and connection resets inside
        # tool_result content (target-program logs, source line numbers)
        # are NOT API failures. Assistant text is what matters.
        self._write("agent/claude-code.txt", [
            self.INIT,
            ev(type="user", message={"role": "user", "content": [{
                "tool_use_id": "Bash_9", "type": "tool_result",
                "content": "429\tstate->resetForNewQuery();\nconnection reset by peer\n"}]}),
            self.RESULT_OK])
        rc, out, _ = run_gate(self.dir)
        self.assertEqual(rc, 0, f"tool-result 429 must pass (got rc={rc}): {out}")

    def test_assistant_text_api_error_blocks(self):
        self._write("agent/claude-code.txt", [self.INIT, ev(
            type="assistant", message={"role": "assistant", "content": [
                {"type": "text", "text": "Hit an API Error: 429 Too Many Requests, backing off"}]}),
            self.RESULT_OK])
        rc, _, err = run_gate(self.dir)
        self.assertEqual(rc, 1, "assistant text naming an API error must block")

    def test_session_jsonl_shapes_checked_but_not_truncation(self):
        # Session logs get error-shape checks but not the truncation rule
        # (their result-event presence is not guaranteed by the format).
        self._write("agent/sessions/abc.jsonl", [self.INIT, self.ASSISTANT])
        rc, out, _ = run_gate(self.dir)
        self.assertEqual(rc, 0, f"session jsonl without result must pass: {out}")
        self._write("agent/sessions/abc.jsonl", [ev(
            type="assistant", message={"role": "assistant", "isApiErrorMessage": True,
                                       "content": []})])
        rc, _, _ = run_gate(self.dir)
        self.assertEqual(rc, 1, "session jsonl with isApiErrorMessage must block")

    def test_empty_dir_passes(self):
        rc, out, _ = run_gate(self.dir)
        self.assertEqual(rc, 0, out)


if __name__ == "__main__":
    unittest.main()
