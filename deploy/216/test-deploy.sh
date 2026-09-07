#!/usr/bin/env bash
set -euo pipefail
bash -n "$(dirname "$0")/deploy.sh"
grep -q 'CGO\|dynamically linked\|no dynamic scheduler plugin' "$(dirname "$0")/deploy.sh"
