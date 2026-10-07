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
exec 9>"$root/.release.lock"
flock -n 9 || { echo "another CPA1 release is active" >&2; exit 1; }
incoming="$root/incoming/$release_name.tar.gz"
release_dir="$root/releases/$release_name"
old_target="$(readlink -f "$root/current" 2>/dev/null || true)"
current_path="$root/current-cpa1"
if [ -L "$current_path" ]; then
  old_target="$(readlink -f "$current_path")"
fi
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

SMOOTH_HELPER="${CPA_216_SMOOTH_HELPER:-$release_dir/smooth-release.py}"
SMOOTH_STATE_DIR="${CPA_216_SMOOTH_STATE_DIR:-$root/incoming/$release_name-state}"
if ! python3 "$SMOOTH_HELPER" cpa1 begin "$release_dir" "$SMOOTH_STATE_DIR"; then
  echo "CPA1 bridge readiness or drain failed; canonical service was not restarted" >&2
  exit 1
fi

ln -sfn "$release_dir" "$current_path.next"
mv -Tf "$current_path.next" "$current_path"
install -d -m 0755 /etc/systemd/system/cliproxyapi.service.d
printf '[Service]\nExecStart=\nExecStart=%s/cli-proxy-api -config /etc/cliproxyapi/config.yaml -local-model\n' \
  "$current_path" > /etc/systemd/system/cliproxyapi.service.d/release-pointer.conf
systemctl daemon-reload
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
  python3 "$SMOOTH_HELPER" cpa1 finish "$release_dir" "$SMOOTH_STATE_DIR" || return 1
}

if ! activate_services; then
  echo "CPA restart or HTTP health verification failed; rolling back" >&2
  if [ -n "$old_target" ]; then
    if ! python3 "$SMOOTH_HELPER" cpa1 hold "$release_dir" "$SMOOTH_STATE_DIR"; then
      echo "CPA1 rollback drain failed; preserve the healthy bridge and current process" >&2
      exit 1
    fi
    ln -sfn "$old_target" "$current_path.next"
    mv -Tf "$current_path.next" "$current_path"
    rollback_ok=true
    for service_port in 'cliproxyapi.service 8317'; do
      read -r service port <<< "$service_port"
      if ! systemctl restart "$service" || ! verify_service "$service" "$port"; then
        rollback_ok=false
      fi
    done
    if [ "$rollback_ok" = false ]; then
      echo "CPA rollback verification failed; manual intervention required" >&2
    elif ! python3 "$SMOOTH_HELPER" cpa1 finish "$release_dir" "$SMOOTH_STATE_DIR"; then
      echo "CPA1 rollback restored HTTP health; bridge cleanup still requires attention" >&2
    fi
  fi
  exit 1
fi
echo "deployed $release_name"
