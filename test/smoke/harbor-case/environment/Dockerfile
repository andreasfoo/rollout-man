# rollout-man never reads this file. Building it is Harbor's job; the case only
# has to declare it, the way every real Harbor case does.
FROM debian:bookworm-slim

RUN mkdir -p /app /logs/agent /logs/verifier \
    && (id -u agent >/dev/null 2>&1 || useradd -m -u 1000 agent) \
    && chown -R agent:agent /app /logs/agent \
    && chmod 755 /logs/verifier

COPY seed/ /app/seed/
WORKDIR /app
