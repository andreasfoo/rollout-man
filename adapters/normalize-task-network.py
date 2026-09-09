#!/usr/bin/env python3
"""Normalize a task.toml's network-mode handling to the canonical published shape.

Replace the section-agnostic `network_mode`/`allow_internet`/`allowed_hosts` line
scrub used by the week3 materializer: instead of dropping every line matching
those keys, rewrite each target section ([verifier] / [agent] / [environment])
to the canonical, non-leaking values the corpus convention requires
(capstone-issue-191-t4 + week1 origin/main reference).

Canonical shape (corpus convention; user-specified, authoritative):

    schema_version = "1.3"          # bumped from "1.2" if present
    [verifier]
    network_mode = "no-network"
    [agent]
    network_mode = "allowlist"
    allowed_hosts = []              # the real allowlist is injected at harbor
    [environment]                   # runtime via --allow-agent-host, not in
    network_mode = "public"         # the published file (proxy IP rule).

Strictly safer than the prior scrub: the proxy/sandbox network config is
overwritten with a fixed literal and `allowed_hosts` is forced to [], so
no live host ever reaches the published file. `allow_internet` is still
dropped entirely (never re-emitted). Gate-safe: no literal emitted here
matches any leak-gate regex (`:12584`, `tingly`, `sk-...`) or contains
an IPv4 address.

Algorithm (pure line-based editing -- no TOML library, matches the
materializer's plain-text convention):
  1. Read lines; bump `schema_version = "1.2"` -> `"1.3"` (only if a
     `schema_version` line exists; leave value alone if already `1.3`).
  2. Walk lines tracking the current top-level section header.
  3. Strip any existing `network_mode` / `allowed_hosts` / `allow_internet`
     line in every section (SCRUB_RE).
  4. When leaving a target section ([verifier] / [agent] / [environment]) --
     at the next top-level [header] or EOF -- append that section's canonical
     lines as its last content, before the blank-line separator. [agent]
     order: `network_mode = "allowlist"` then `allowed_hosts = []`.
  5. Inject only into sections that already exist. All 18 week3 tomls have
     all three; do NOT synthesize a missing section, do NOT add
     [verifier.environment] (the capstone example has only the three).

Idempotent by construction: strip-then-append + guarded schema bump means
re-running yields identical bytes (required because watch re-gates a case
on any content change).

Usage:
    normalize-task-network.py <path-to-task.toml> [<more-paths>...]

On any error, exit non-zero without writing. On success, the file is
rewritten in place; if the file already matches the canonical shape, a
no-op write still occurs (idempotence test passes when the second call
produces identical bytes).
"""
import os
import re
import sys

# Strip set matches the prior scrub (SCRUB_KEYS in the materializer
# adapters); reusing it keeps the strip behavior consistent for keys
# that no longer belong in any section.
SCRUB_RE = re.compile(r'\s*(network_mode|allow_internet|allowed_hosts)\s*=')
SCHEMA_RE = re.compile(r'^(schema_version\s*=\s*")(\d+\.\d+)("[^\n]*)')

TARGET_SECTIONS = {
    'verifier': ['network_mode = "no-network"'],
    'agent':    ['network_mode = "allowlist"', 'allowed_hosts = []'],
    'environment': ['network_mode = "public"'],
}

# ANY section header, dotted sub-tables ([verifier.env], [solution.env])
# included. A sub-table header is exactly where the parent table's DIRECT
# keys stop: everything after `[environment.env]` belongs to
# `environment.env`, not to `environment`. So a header is both the flush
# point for the section we are leaving AND (when dotted) a non-target
# section we must not inject into.
#
# Getting this wrong is silent and total: with a flush deferred to the next
# TOP-LEVEL header, a task.toml whose [environment] is followed by
# [environment.env] (and no further top-level section) emits
# `network_mode = "public"` at EOF -- i.e. INSIDE [environment.env], parsing
# as `environment.env.network_mode` while the key Harbor actually reads,
# `environment.network_mode`, stays absent. Valid TOML, right-looking diff,
# zero effect. Flush at every header instead.
ANY_HEADER_RE = re.compile(r'^\s*\[([^\[\]]+)\]\s*(?:#.*)?$')


def normalize(path):
    if not os.path.isfile(path):
        raise SystemExit(f"normalize: not a file: {path}")
    with open(path) as f:
        lines = f.read().splitlines(keepends=True)

    out = []
    cur = None              # current top-level section name, or None
    pending = []            # canonical lines to flush when current section ends

    def flush():
        # Canonical lines become the section's last CONTENT line -- inserted
        # before whatever blank-line separator the file already uses, so the
        # section/blank/section rhythm survives and re-running is a no-op.
        if not pending:
            return
        blanks = []
        while out and not out[-1].strip():
            blanks.append(out.pop())
        out.extend(pending)
        out.extend(reversed(blanks))
        pending.clear()

    for line in lines:
        # Bump schema_version "1.2" -> "1.3" in-place. A simple string replace
        # is safer than a regex here: the regex's capture-group suffix has to
        # be either greedy (which would include characters past the closing
        # quote) or non-newline-only (which would drop the line's trailing
        # newline), and either trap loses or alters the file's whitespace.
        # `splitlines(keepends=True)` keeps the line's own newline in `line`,
        # so a `replace` of just the quoted version substring preserves it.
        m = SCHEMA_RE.match(line)
        if m:
            if m.group(2) == "1.2":
                out.append(line.replace('"1.2"', '"1.3"', 1))
            else:
                out.append(line)
            continue

        h = ANY_HEADER_RE.match(line)
        if h:
            # A header -- of any depth -- ends the preceding table's direct
            # keys, so this is the flush point.
            flush()
            # Only an undotted header names a table we inject into; a dotted
            # sub-table sets cur to None so its content lines never arm
            # `pending` (see the ANY_HEADER_RE note).
            name = h.group(1).strip()
            cur = name if '.' not in name else None
            out.append(line)
            continue

        # Drop any line matching the strip set in every section (the
        # injector below writes the canonical replacement, and the
        # strip is the same regardless of section).
        if SCRUB_RE.match(line):
            continue

        out.append(line)
        if cur in TARGET_SECTIONS and not pending:
            # We just appended a content line inside a target section.
            # Set up the canonical lines to flush when we leave the
            # section. (We could flush now, but flushing on exit keeps
            # the canonical lines as the section's last content even if
            # the original section had trailing content; that matches
            # the capstone shape where network_mode IS the last line.)
            pending = [ln + '\n' for ln in TARGET_SECTIONS[cur]]

    # End of file: flush any pending canonical lines.
    flush()

    new = ''.join(out)
    with open(path, 'w') as f:
        f.write(new)


def main(argv):
    if len(argv) < 2:
        raise SystemExit("usage: normalize-task-network.py <task.toml> [<more>...]")
    for p in argv[1:]:
        normalize(p)


if __name__ == '__main__':
    main(sys.argv)
