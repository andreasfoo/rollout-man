#!/usr/bin/env bash
# Build a minimal base image out of the host's own bash and coreutils.
#
# The docker step of the smoke test needs *a* base image; which one is
# irrelevant to what it proves. On a machine that cannot reach an image
# registry this builds one locally so the step still runs instead of being
# skipped and quietly counted as passing.
set -euo pipefail
IMAGE=${1:-rollout-man-smoke-base:1}
R=$(mktemp -d)
trap 'rm -rf "$R"' EXIT

mkdir -p "$R"/{usr/bin,usr/sbin,lib64,etc,app,logs/agent,logs/verifier,solution,tests,tmp,root,home/agent}
for b in bash sh cat sleep timeout mkdir cp mv rm ls id echo printf chmod chown touch grep sed env true false dirname basename head tail wc sort ln; do
  p=$(command -v "$b" 2>/dev/null) || continue
  cp -aL "$p" "$R/usr/bin/$b" 2>/dev/null || true  # -L: /bin/sh is a symlink; the target is what has to be in the image
done
ln -s usr/bin "$R/bin"; ln -s usr/sbin "$R/sbin"
for p in "$R"/usr/bin/*; do ldd "$p" 2>/dev/null; done |
  awk '{for(i=1;i<=NF;i++) if($i ~ /^\//) print $i}' | sort -u |
  while read -r l; do
    rp=$(readlink -f "$l")
    for t in "$l" "$rp"; do d="$R$(dirname "$t")"; mkdir -p "$d"; cp -fL "$rp" "$d/$(basename "$t")"; done
  done
cp -fL /lib64/ld-linux-x86-64.so.2 "$R/lib64/ld-linux-x86-64.so.2" 2>/dev/null || true

printf 'root:x:0:0:root:/root:/bin/bash\nagent:x:1000:1000:agent:/home/agent:/bin/bash\n' > "$R/etc/passwd"
printf 'root:x:0:\nagent:x:1000:\n' > "$R/etc/group"
chown -R 1000:1000 "$R/home/agent" "$R/app" "$R/logs/agent"
chmod 1777 "$R/tmp"; chmod 755 "$R/logs/verifier"

tar -C "$R" -c . | docker import \
  -c 'ENV PATH=/usr/bin:/bin:/usr/sbin:/sbin' -c 'WORKDIR /app' - "$IMAGE" > /dev/null
echo "$IMAGE"
