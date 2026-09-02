"""Regression tests for adapters/check-no-chinese-traj.sh's two-tier CJK gate.

The gate exists because kimi-k3 sometimes answers in Chinese and that must
not ship to the English-only dataset. Its first iteration failed any file
containing a CJK codepoint -- including raw x86 disassembly dumps whose
instruction bytes decode to CJK glyphs (clickhouse-e9a9df4, 2026-08-31).
These tests pin the two-tier behavior: binary-dump mojibake passes, real
Chinese text blocks.
"""
import os
import subprocess
import tempfile
import unittest

ADAPTER = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check-no-chinese-traj.sh")
MODEL = os.path.expanduser("~/.cache/rollout-man/lid.176.ftz")

MODEL_AVAILABLE = os.path.isfile(MODEL)
try:
    import fasttext  # noqa: F401
    FASTTEXT_AVAILABLE = True
except ImportError:
    FASTTEXT_AVAILABLE = False


def run_gate(traj_dir):
    """Run the adapter against a directory; return (exit_code, stdout, stderr)."""
    env = dict(os.environ, TRAJ_DIR=traj_dir)
    p = subprocess.run(["bash", ADAPTER], env=env, capture_output=True, text=True, timeout=120)
    return p.returncode, p.stdout, p.stderr


class CjkGateTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()

    def _write(self, name, text):
        path = os.path.join(self.dir, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write(text)

    def test_pure_english_passes(self):
        self._write("a.txt", "the bug is in tga.c line 377, CWE-787\nordinary log line\n")
        rc, out, _ = run_gate(self.dir)
        self.assertEqual(rc, 0, out)

    def test_english_with_cjk_lookalikes_passes(self):
        # "interestingly" contains no CJK codepoint but did trip the OLD
        # leak gate's tingly pattern; here it must pass the CJK gate itself.
        self._write("a.txt", "No git metadata, and interestingly the source tree is trimmed\n")
        rc, out, _ = run_gate(self.dir)
        self.assertEqual(rc, 0, out)

    @unittest.skipUnless(MODEL_AVAILABLE and FASTTEXT_AVAILABLE, "fasttext+model needed")
    def test_disassembly_dump_mojibake_passes(self):
        # The regression: x86 objdump output whose byte pairs decode to CJK
        # glyphs. UAWAVAUATSH is the classic System-V prologue opcode run;
        # the spaces stand in for the \x00 padding the real dump had.
        dump = (
            "UAWAVAUATSH藞 I $H蛟 H=\n"
            "H 謙 艙 L9\n"
        )
        self._write("agent/claude-code.txt", '{"type":"user","content":"' + dump + '"}')
        rc, out, _ = run_gate(self.dir)
        self.assertEqual(rc, 0, f"binary dump must pass (got rc={rc}): {out}")

    @unittest.skipUnless(MODEL_AVAILABLE and FASTTEXT_AVAILABLE, "fasttext+model needed")
    def test_real_chinese_blocks(self):
        self._write("agent/trajectory.json", '{"message":"请注意：此文件包含敏感信息，禁止上传。"}')
        rc, _, err = run_gate(self.dir)
        self.assertEqual(rc, 1, "real Chinese must block")
        self.assertIn("Chinese text", err)

    @unittest.skipUnless(MODEL_AVAILABLE and FASTTEXT_AVAILABLE, "fasttext+model needed")
    def test_short_chinese_greeting_blocks(self):
        # Two chars is the hardest case for language ID; a leak this small
        # still must not ship.
        self._write("a.txt", "assistant said: 你好 and nothing else\n")
        rc, _, err = run_gate(self.dir)
        self.assertEqual(rc, 1, f"short Chinese must block (got rc={rc})")

    # --- Tier 2a: deterministic gb2312 block (model-free) ---------------
    # These short runs classify as `en` under lid.176 (几十 -> en 0.13,
    # 复活/大概率 -> en ~0.30) because the model is unreliable below ~10
    # chars -- the 2026-09-01 leak. They are all common gb2312 characters,
    # so the deterministic tier must block them WITHOUT consulting fasttext.
    # No skipUnless: the whole point is that tier 2a needs no model.
    def test_short_chinese_jishi_blocks_without_model(self):
        # 几十 = "several tens of", the run that actually leaked inside an
        # otherwise-English sentence.
        self._write("a.txt", "it retries 几十 times before giving up\n")
        rc, _, err = run_gate(self.dir)
        self.assertEqual(rc, 1, f"几十 must block via gb2312 tier (got rc={rc})")
        self.assertIn("Chinese text", err)

    def test_short_chinese_fuhuo_blocks_without_model(self):
        self._write("a.txt", "the process will 复活 after the crash\n")
        rc, _, err = run_gate(self.dir)
        self.assertEqual(rc, 1, f"复活 must block via gb2312 tier (got rc={rc})")

    def test_short_chinese_dagailv_blocks_without_model(self):
        self._write("a.txt", "this is 大概率 a use-after-free\n")
        rc, _, err = run_gate(self.dir)
        self.assertEqual(rc, 1, f"大概率 must block via gb2312 tier (got rc={rc})")

    def test_single_cjk_char_falls_through_to_model(self):
        # A lone CJK codepoint is NOT >=2 gb2312 chars, so tier 2a does not
        # fire and it falls through to lid.176 -- the binary-dump path. This
        # pins that the deterministic tier is scoped to >=2-char runs and
        # doesn't turn every stray glyph into an unconditional block.
        self._write("a.txt", "opcode row 藞 in the disassembly\n")
        rc, out, err = run_gate(self.dir)
        if MODEL_AVAILABLE and FASTTEXT_AVAILABLE:
            self.assertEqual(rc, 0, f"single rare glyph should pass model (got rc={rc}): {err}")
        else:
            # No model -> residual adjudication fails closed. Either way tier
            # 2a must not have fired on a single char.
            self.assertEqual(rc, 1, out)

    def test_binary_file_skipped(self):
        # NUL in the first 8KB -> treated as binary, never scanned.
        with open(os.path.join(self.dir, "blob.bin"), "wb") as f:
            f.write(b"\x00\x01\x02\x85\xde\x86\xdf")  # would decode to 藞蛟
        rc, out, _ = run_gate(self.dir)
        self.assertEqual(rc, 0, out)

    def test_empty_dir_passes(self):
        rc, out, _ = run_gate(self.dir)
        self.assertEqual(rc, 0, out)

    def test_missing_fasttext_fails_closed(self):
        # CJK present + fasttext unimportable => block, never silently pass.
        # A bogus PYTHONPATH does not hide the real fasttext from the
        # interpreter's default path, so what this pins is simply: the gate
        # never exits 0 with CJK present (whether it blocks on the language
        # check or on the missing-dep fallback).
        self._write("a.txt", "assistant said: 你好\n")
        env = dict(os.environ, TRAJ_DIR=self.dir,
                   PYTHONPATH="/nonexistent-no-fasttext-here")
        env.pop("PYTHONHOME", None)
        p = subprocess.run(["bash", ADAPTER], env=env, capture_output=True, text=True, timeout=120)
        self.assertEqual(p.returncode, 1,
                         f"must block (got rc={p.returncode}): {p.stdout}{p.stderr}")


if __name__ == "__main__":
    unittest.main()
