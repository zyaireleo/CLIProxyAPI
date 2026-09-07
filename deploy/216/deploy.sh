#!/usr/bin/env bash
set -euo pipefail

release_name="${1:-}"
archive_path="${2:-}"
archive_sha256="${3:-}"

case "$release_name" in
  ''|*[!A-Za-z0-9._-]*) echo "invalid release name" >&2; exit 2 ;;
esac
test -f "$archive_path"
test "$archive_sha256" = "$(sha256sum "$archive_path" | awk '{print $1}')"

root=/opt/cliproxyapi
incoming="$root/incoming/$release_name.tar.gz"
release_dir="$root/releases/$release_name"
old_target="$(readlink -f "$root/current" 2>/dev/null || true)"

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
printf 'release=%s\narchive_sha256=%s\n' "$release_name" "$archive_sha256" > "$release_dir/BUILDINFO"

ln -sfn "$release_dir" "$root/current.next"
mv -Tf "$root/current.next" "$root/current"
if ! systemctl restart cliproxyapi.service || ! systemctl is-active --quiet cliproxyapi.service; then
  if [ -n "$old_target" ]; then
    ln -sfn "$old_target" "$root/current.next"
    mv -Tf "$root/current.next" "$root/current"
    systemctl restart cliproxyapi.service || true
  fi
  exit 1
fi
systemctl show -p NRestarts --value cliproxyapi.service | grep -qx '0'
echo "deployed $release_name"
