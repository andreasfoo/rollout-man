# The base is rewritten by test/smoke/run.sh so the step can run against a
# locally built rootfs when the image registry is unreachable.
FROM debian:bookworm-slim

RUN mkdir -p /app /logs/agent /logs/verifier \
    && (id -u agent >/dev/null 2>&1 || useradd -m -u 1000 agent) \
    && chown -R agent:agent /app /logs/agent \
    && chmod 755 /logs/verifier

COPY seed/ /app/seed/
WORKDIR /app
