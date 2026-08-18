# SpiderMonkey js-shell wasm lazy-entry-stub memset WRITE overrun — env.
#
# Two-stage build (B1 shared-source architecture): stage 1 materializes THIS
# case's exact source tree from cyborgzero/sm-srcbase:latest (FULL non-promisor
# Firefox .git, all blobs local) via `git checkout <VULN_SHA> && rm -rf .git`
# (offline, no lazy-fetch). Stage 2 is the pre-built js binary
# (sm-base:3b7743c); we COPY /src/firefox from stage 1 for the agent's
# clue-search under /src/firefox/js/src/.
#
# The binary is pre-built on the host (parent 3b7743cd8af6..., 2024-11-07,
# ASan clang-21 + debug symbols, non-debug) and shipped via sm-base:3b7743c.

# --- stage 1: materialize the exact vulnerable source tree ---
FROM cyborgzero/sm-srcbase:latest AS src
WORKDIR /src/firefox
RUN git checkout -q 3b7743cd8af65ec7247ea744e61ad810075b1cde \
    && rm -rf /src/firefox/.git

# --- stage 2: pre-built binary + the source tree from stage 1 ---
FROM cyborgzero/sm-base:3b7743c
COPY --from=src /src/firefox /src/firefox

ENV DEBIAN_FRONTEND=noninteractive
# gdb is present in the base image; reinstall defensively.
RUN apt-get update && apt-get install -y --no-install-recommends gdb && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /app /logs/verifier /logs/agent
ENV ASAN_OPTIONS="detect_leaks=0:abort_on_error=1:halt_on_error=1:symbolize=1:allocator_may_return_null=1"

# Agent-facing clue material (human-curated hints, NOT the PoC).
COPY seed/ /app/seed/

# Runtime isolation: agent writes only deliverables; verifier + oracle root-owned.
RUN (id -u agent >/dev/null 2>&1 || useradd -m -s /bin/bash agent) \
    && chown -R agent:agent /app /logs/agent \
    && chmod 755 /logs/verifier \
    && chown -R root:root /opt/oracle && chmod -R go-w /opt/oracle

WORKDIR /app
