# libaom AV1 decoder — entropy-coder heap-buffer-overflow READ defect — env.
#
# This task follows the pipeline's host-build → DockerHub base-image flow:
#   - the vulnerable `av1_dec_fuzzer` harness was built ON THE HOST with
#     clang-15 + ASan against libaom parent cc9e46cb0da9323ecacf679772f6f5b21934a363
#     (2024-03-05), ASan statically linked;
#   - the prebuilt binary + the exact source tree are shipped in the base image
#     `cyborgzero/aom-base:cc9e46cb`;
#   - this Dockerfile only FROMs that image and adds the agent-facing layout.
#     NO in-image CMake build (the ~2min libaom build already happened on host).
# See memory libaom-av1-decoder-case-validated + spidermonkey-host-build-ship-binary.
#
# The harness is a file-driven driver over the oss-fuzz `av1_dec_fuzzer`
# contract: it reads the PoC blob into memory and calls
# `LLVMFuzzerTestOneInput(buf, size)`, which decodes the IVF-wrapped AV1
# bitstream frame-by-frame. A malformed tile-group OBU drives the entropy
# decoder (`od_ec_dec_refill`) to read past the 194-byte input region →
# ASan heap-buffer-overflow READ at aom_dsp/entdec.c:94.

FROM cyborgzero/aom-base:cc9e46cb

ENV DEBIAN_FRONTEND=noninteractive

# Agent deliverable parent + oracle + log dirs.
RUN mkdir -p /app /logs/verifier /logs/agent
ENV ASAN_OPTIONS="detect_leaks=0:abort_on_error=0:halt_on_error=1:symbolize=1:allocator_may_return_null=1"

# Agent-facing clue material (human-curated hints, NOT the PoC).
COPY seed/ /app/seed/

# Runtime isolation: agent writes only deliverables; verifier + oracle root-owned.
RUN (id -u agent >/dev/null 2>&1 || useradd -m -s /bin/bash agent) \
    && chown -R agent:agent /app /logs/agent \
    && chmod 755 /logs/verifier \
    && chown -R root:root /opt/oracle && chmod -R go-w /opt/oracle

WORKDIR /app
