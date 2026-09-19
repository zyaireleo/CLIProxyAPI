#!/usr/bin/env bash
set -euo pipefail
script_dir="$(cd "$(dirname "$0")" && pwd)"
bash -n "$script_dir/deploy.sh"
grep -q 'dynamically linked' "$script_dir/deploy.sh"
grep -q 'refusing to replace the active release directory' "$script_dir/deploy.sh"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
cat > "$fixture/check.sh" <<'CHECK'
#!/usr/bin/env bash
set -euo pipefail
root="$TEST_ROOT"
release_dir="$root/new"
old_target="$root/old"
release_name=candidate
systemctl() {
  case "$1" in
    restart)
      count=0
      [ ! -f "$root/restarts" ] || count="$(cat "$root/restarts")"
      count=$((count + 1))
      echo "$count" > "$root/restarts"
      if [ "$SCENARIO" = restart-failure ] && [ "$count" = 1 ]; then return 1; fi
      ;;
    is-active) return 0 ;;
    show)
      if [ "$SCENARIO" = crash-loop ] && [ "$(readlink "$root/current")" = "$release_dir" ]; then
        echo 1
      else
        echo 0
      fi
      ;;
  esac
}
curl() {
  echo "$*" >> "$root/probes"
  if [ "$SCENARIO" = rollback-failure ]; then return 1; fi
  if [ "$SCENARIO" = http-failure ] && [ "$(readlink "$root/current")" = "$release_dir" ]; then return 1; fi
  if [ "$SCENARIO" = rollback-delayed ] && [ "$(readlink "$root/current")" = "$release_dir" ]; then return 1; fi
  if [ "$SCENARIO" = rollback-delayed ] && [ ! -f "$root/rollback-probed" ]; then
    touch "$root/rollback-probed"
    return 1
  fi
  return 0
}
sleep() { :; }
mv() {
  if [ "$1" = -Tf ]; then shift; fi
  python3 -c 'import os,sys; os.replace(sys.argv[1], sys.argv[2])' "$@"
}
CHECK
sed -n '/^ln -sfn "$release_dir"/,$p' "$script_dir/deploy.sh" >> "$fixture/check.sh"
for scenario in success restart-failure http-failure crash-loop rollback-failure rollback-delayed; do
  root="$fixture/$scenario"
  mkdir -p "$root/old" "$root/new"
  ln -s "$root/old" "$root/current"
  status=0
  TEST_ROOT="$root" SCENARIO="$scenario" bash "$fixture/check.sh" > "$root/result" 2>&1 || status=$?
  if [ "$scenario" = success ]; then
    test "$status" = 0
    test "$(readlink "$root/current")" = "$root/new"
    grep -q '/healthz' "$root/probes"
  else
    test "$status" != 0
    test "$(readlink "$root/current")" = "$root/old"
    test "$(cat "$root/restarts")" = 2
    if [ "$scenario" = rollback-failure ]; then
      grep -q 'manual intervention required' "$root/result"
    else
      ! grep -q 'manual intervention required' "$root/result"
    fi
  fi
  echo "PASS: $scenario"
done
