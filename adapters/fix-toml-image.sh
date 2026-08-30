#!/usr/bin/env bash
# Per-case auto-fix for a failing check_toml_image verdict.
#
# check_toml_image rejects a case whose task.toml has no docker_image field.
# The only legitimate repair is the full publish flow the check is standing in
# for: build the image from the case's own environment/, push it to the
# registry, and backfill docker_image into task.toml. The retried
# check_toml_image (fix-and-retry) is the real validation: it re-reads
# task.toml and sees the backfilled field or the case stays rejected.
#
# Naming follows the sibling admitted cases: cyborgzero/<case-dir-name>:latest
# (e.g. cyborgzero/nginx-6446f99-commit-bcb41c9-t4:latest). The image is built
# from environment/ as the docker build context, so every COPY the Dockerfile
# names must exist there -- a case whose build context is incomplete (gitignored
# artifacts never committed) fails the build loudly rather than shipping an
# image that cannot be reproduced.
#
# ENV (all passed through rollout-man; see internal/actions/actions.go runFix):
#   CASE_DIR     path to the case package (writable; backfill edits task.toml)
#   CASE_LABEL   human-readable label for messages
#   REGISTRY_USER  (opt) registry namespace, default cyborgzero
set -euo pipefail

: "${CASE_DIR:?fix_toml_image needs CASE_DIR}"
: "${CASE_LABEL:=fix_toml_image}"

user="${REGISTRY_USER:-cyborgzero}"
name="$(basename "$CASE_DIR")"
image="$user/$name:latest"
toml="$CASE_DIR/task.toml"
envdir="$CASE_DIR/environment"

[ -f "$toml" ] || { printf 'fix_toml_image: FAIL %s -- no task.toml\n' "$CASE_LABEL" >&2; exit 1; }
[ -f "$envdir/Dockerfile" ] || {
  printf 'fix_toml_image: FAIL %s -- no environment/Dockerfile to build\n' "$CASE_LABEL" >&2
  exit 1
}

printf 'fix_toml_image: %s -- building %s from %s (this can take a while)\n' "$CASE_LABEL" "$image" "$envdir"
docker build -t "$image" "$envdir" || {
  printf 'fix_toml_image: FAIL %s -- docker build failed\n' "$CASE_LABEL" >&2
  exit 1
}

docker push "$image" || {
  printf 'fix_toml_image: FAIL %s -- docker push %s failed\n' "$CASE_LABEL" "$image" >&2
  exit 1
}

# Pin to the digest, not the tag: Harbor re-pulls the tag, and a :latest that
# someone else re-pushes would silently change the case's environment (the
# stale-:latest varnish regression). The digest is the same toml form the
# varnish fix landed in.
digest=$(docker image inspect "$image" --format '{{index .RepoDigests 0}}')
[ -n "$digest" ] || {
  printf 'fix_toml_image: FAIL %s -- could not read pushed digest for %s\n' "$CASE_LABEL" "$image" >&2
  exit 1
}

python3 - "$toml" "$digest" <<'PY'
import sys

# Insert docker_image under [environment] -- the section Harbor's
# task_env_config.docker_image reads (definition.py: should_use_prebuilt...
# is only consulted for the [environment] key; a docker_image under [task]
# is silently ignored and Harbor rebuilds the environment from its Dockerfile,
# the very thing publishing the image was supposed to avoid). An existing
# docker_image line (wherever it sits) is replaced in place rather than
# duplicated.
toml_path, digest = sys.argv[1:]
lines = open(toml_path).read().splitlines(keepends=True)
out, replaced = [], False
for line in lines:
    if line.lstrip().startswith('docker_image') and '=' in line:
        out.append(f'docker_image = "{digest}"\n')
        replaced = True
    else:
        out.append(line)
        if not replaced and line.strip() == '[environment]':
            out.append(f'docker_image = "{digest}"\n')
            replaced = True
if not replaced:
    # No [environment] section at all: append one (a case without environment
    # config would not have passed check_toml_image's environment/ check, but
    # be defensive rather than silently dropping the backfill).
    out.append(f'\n[environment]\ndocker_image = "{digest}"\n')
open(toml_path, 'w').writelines(out)
PY

printf 'fix_toml_image: %s -- pushed %s, task.toml docker_image = %s\n' \
  "$CASE_LABEL" "$image" "$digest"
