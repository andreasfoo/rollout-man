FROM debian:12-slim
RUN mkdir -p /app /logs/verifier /logs/agent \
    && (id -u agent >/dev/null 2>&1 || useradd -m agent) \
    && chown -R agent:agent /app /logs/agent
WORKDIR /app
