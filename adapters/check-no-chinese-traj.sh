#!/usr/bin/env bash
# CJK gate for trajectory trees, language-detection edition (2026-08-31).
#
# The old gate failed any file containing a CJK codepoint -- including raw
# x86 disassembly dumps whose instruction bytes happen to decode to CJK
# glyphs (clickhouse-e9a9df4 trial: objdump output contained 藞/蛟/謙 from
# \xde\x85-style byte pairs, not actual Chinese). The benchmark IS
# English-only, so real Chinese text in a trajectory is a leak and must not
# ship; the fix is to keep the cheap CJK-codepoint gate but let a
# language-ID model (fasttext lid.176) adjudicate the false positives.
#
# Two-tier logic per file:
#   1. No CJK codepoint anywhere            -> pass (cheap, unchanged path)
#   2. CJK found -> for each CJK run:
#      a. a run of >=2 chars that are ALL GB2312-encodable (common modern
#         simplified Chinese) is real Chinese -- block unconditionally. This
#         is the deterministic tier, added 2026-09-01 after a 2-char leak
#         (几十, "several tens of", inside an English sentence) shipped: it
#         classified as `en` 0.13 because lid.176 is unreliable on inputs
#         shorter than ~10 chars, and the sub-0.70 confidence passed it.
#         Real Chinese words are common characters; the gb2312 test catches
#         them without a length-sensitive model in the loop.
#      b. otherwise (single char, or any char outside gb2312 -- i.e. the
#         rare/random codepoints a binary dump produces) fall through to
#         lid.176: block only if it classifies as Chinese (zh/zh_Hans/
#         zh_Hant) at confidence > 0.7. Binary dumps classify as a random
#         low-confidence language (the original blocked trajectory's dump
#         read "lb" at 0.61) and pass.
#
# ENV (set by rollout-man):
#   TRAJ_DIR   the materialized trajectory tree to scan
# Model at $HOME/.cache/rollout-man/lid.176.ftz (downloaded once from
# dl.fbaipublicfiles.com). fasttext is a pip package.
set -euo pipefail

: "${TRAJ_DIR:?check-no-chinese-traj needs TRAJ_DIR}"

python3 - "$TRAJ_DIR" <<'PY'
import os, re, sys
traj = sys.argv[1]
CJK = re.compile(r"[㐀-䶿一-鿿豈-﫿　-〿＀-￠]")

# Pass 1: any CJK at all? Cheap regex over every text file.
candidates = []  # (relpath, [(lineno, line), ...])
for root, dirs, files in os.walk(traj):
    for fn in sorted(files):
        p = os.path.join(root, fn)
        try:
            data = open(p, "rb").read()
        except OSError:
            continue
        if b"\x00" in data[:8192]:
            continue  # binary
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            continue
        hits = [(i, line) for i, line in enumerate(text.splitlines(), 1) if CJK.search(line)]
        if hits:
            candidates.append((os.path.relpath(p, traj), hits))

if not candidates:
    print(f"check_no_chinese_traj: ok ({traj}) -- no CJK codepoints")
    sys.exit(0)

CJK_RUN = re.compile(r"[㐀-鿿　-〿＀-￠]+")

def is_common_zh(ch):
    # GB2312-encodable == a standard modern simplified char (~6763 in levels
    # 1+2). Real Chinese words are built from these; the rare/random
    # codepoints a byte-dump decodes to are almost never gb2312-encodable.
    try:
        ch.encode("gb2312")
        return True
    except UnicodeEncodeError:
        return False

# Tier 2a (deterministic, model-free): a CJK run of >=2 chars that are ALL
# common gb2312 characters is real Chinese -- block regardless of any
# language model. Runs down two failure modes at once: it catches short
# genuine Chinese (几十/复活/大概率) that lid.176 is too short-context to
# score, and it does not depend on fasttext being installed to do so.
def deterministic_zh(hits):
    out = []
    for lineno, line in hits:
        for run in CJK_RUN.findall(line):
            r = run.replace("\x00", "").strip()
            if len(r) >= 2 and all(is_common_zh(c) for c in r):
                out.append((lineno, run, line))
                break
    return out

det_blocked = []
residual = []  # (rel, hits) that still need model adjudication
for rel, hits in candidates:
    d = deterministic_zh(hits)
    if d:
        for lineno, run, line in d:
            det_blocked.append((rel, lineno, "zh", 1.0, line.strip()[:80]))
    else:
        residual.append((rel, hits))

# Pass 2b: language-ID adjudication of the residual (single-char / rare-
# codepoint runs -- the binary-dump mojibake case). If fasttext or its model
# is missing the gate fails closed, so a missing dep can never let a leak
# through.
model = None
if residual:
    try:
        import fasttext
    except ImportError:
        print("check_no_chinese_traj: FAIL -- CJK found but fasttext unavailable; refusing to adjudicate", file=sys.stderr)
        sys.exit(1)
    model_path = os.path.expanduser("~/.cache/rollout-man/lid.176.ftz")
    if not os.path.isfile(model_path):
        print(f"check_no_chinese_traj: FAIL -- CJK found but {model_path} missing", file=sys.stderr)
        sys.exit(1)
    model = fasttext.load_model(model_path)

ZH_LABELS = {"zh", "zh_Hans", "zh_Hant"}
blocked = list(det_blocked)
for rel, hits in residual:
    for lineno, line in hits:
        # Classify the CJK runs, not the whole line: "assistant said: 你好
        # and nothing else" is 2 CJK chars in 38 English ones and classifies
        # as `en` if you feed the line whole -- a 2-char leak would slip
        # through. The runs alone are what must not be Chinese.
        for run in CJK_RUN.findall(line):
            flat = run.replace("\x00", " ").strip()
            if not flat:
                continue
            labels, probs = model.predict(flat)
            lang = labels[0].replace("__label__", "")
            conf = float(probs[0])
            if lang in ZH_LABELS and conf > 0.7:
                blocked.append((rel, lineno, lang, conf, line.strip()[:80]))
                break  # one blocked run is enough for the line
        # (a line with no zh-classified run contributes nothing)

if blocked:
    print(f"check_no_chinese_traj: FAIL -- Chinese text in {len(blocked)} line(s):", file=sys.stderr)
    for rel, ln, lang, conf, ex in blocked[:10]:
        print(f"  {rel}:{ln} [{lang} {conf:.2f}]: {ex}", file=sys.stderr)
    sys.exit(1)

n = sum(len(h) for _, h in candidates)
print(f"check_no_chinese_traj: ok ({traj}) -- {n} CJK-codepoint line(s) in {len(candidates)} file(s), none are Chinese (binary-dump mojibake)")
PY
