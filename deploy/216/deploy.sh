#!/usr/bin/env bash
set -euo pipefail

release_name="${1:-}"
archive_path="${2:-}"
archive_sha256="${3:-}"
commit_sha="${4:-}"
[[ "$commit_sha" =~ ^[0-9a-f]{40}$ ]] || { echo 'invalid commit sha' >&2; exit 2; }

case "$release_name" in
  ''|*[!A-Za-z0-9._-]*) echo "invalid release name" >&2; exit 2 ;;
esac
test -f "$archive_path"
test "$archive_sha256" = "$(sha256sum "$archive_path" | awk '{print $1}')"

root=/opt/cliproxyapi
incoming="$root/incoming/$release_name.tar.gz"
release_dir="$root/releases/$release_name"
old_target="$(readlink -f "$root/current" 2>/dev/null || true)"
if [ "$old_target" = "$release_dir" ]; then
  echo "refusing to replace the active release directory" >&2
  exit 1
fi

install -o cliproxyapi -g cliproxyapi -m 0600 "$archive_path" "$incoming"
tmp_dir="$root/releases/.${release_name}.tmp.$$"
rm -rf "$tmp_dir"
mkdir -p "$tmp_dir"
trap 'rm -rf "$tmp_dir"' EXIT
tar -xzf "$incoming" -C "$tmp_dir"
test -x "$tmp_dir/cli-proxy-api"

if ! file "$tmp_dir/cli-proxy-api" | grep -q 'dynamically linked'; then
  echo "CPA binary is not dynamically linked" >&2
  exit 1
fi
ldd "$tmp_dir/cli-proxy-api" >/dev/null

rm -rf "$release_dir"
mv "$tmp_dir" "$release_dir"
chown -R cliproxyapi:cliproxyapi "$release_dir"
chmod 0755 "$release_dir/cli-proxy-api"
sha256sum "$release_dir/cli-proxy-api" > "$release_dir/SHA256SUMS"
printf 'release=%s\ncommit=%s\narchive_sha256=%s\nbinary_sha256=%s\n' \
  "$release_name" "$commit_sha" "$archive_sha256" "$(sha256sum "$release_dir/cli-proxy-api" | awk '{print $1}')" > "$release_dir/BUILDINFO"

ln -sfn "$release_dir" "$root/current.next"
mv -Tf "$root/current.next" "$root/current"
verify_service() {
  local attempt
  local service="$1" port="$2"
  for attempt in {1..15}; do
    if systemctl is-active --quiet "$service" &&
      systemctl show -p NRestarts --value "$service" | grep -qx '0' &&
      curl --fail --silent --show-error --max-time 2 "http://127.0.0.1:${port}/healthz" >/dev/null; then
      return 0
    fi
    sleep 2
  done
  return 1
}

activate_services() {
  systemctl restart cliproxyapi.service && verify_service cliproxyapi.service 8317 || return 1
  systemctl restart cliproxyapi2.service && verify_service cliproxyapi2.service 8318 || return 1
}

if ! activate_services; then
  echo "CPA restart or HTTP health verification failed; rolling back" >&2
  if [ -n "$old_target" ]; then
    ln -sfn "$old_target" "$root/current.next"
    mv -Tf "$root/current.next" "$root/current"
    rollback_ok=true
    for service_port in 'cliproxyapi.service 8317' 'cliproxyapi2.service 8318'; do
      read -r service port <<< "$service_port"
      if ! systemctl restart "$service" || ! verify_service "$service" "$port"; then
        rollback_ok=false
      fi
    done
    if [ "$rollback_ok" = false ]; then
      echo "CPA rollback verification failed; manual intervention required" >&2
    fi
  fi
  exit 1
fi
echo "deployed $release_name"
