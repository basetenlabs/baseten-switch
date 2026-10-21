#!/usr/bin/env bash
# Check the compiler recorded in a Go executable, rather than the host compiler.
set -euo pipefail

[[ $# == 1 ]] || { printf 'Usage: %s EXECUTABLE\n' "$0" >&2; exit 1; }
build_info="$("${GO:-go}" version -m "$1")" || exit 1
version="$(printf '%s\n' "$build_info" | awk 'NR == 1 {print $NF}')"
if [[ "$version" =~ ^go1\.([0-9]+)\.([0-9]+)$ ]]; then
  minor="${BASH_REMATCH[1]}"
  patch="${BASH_REMATCH[2]}"
  if (( minor > 27 || (minor == 27 && patch >= 3) || (minor == 26 && patch >= 10) )); then
    printf 'PASS: %s built with %s\n' "$1" "$version"
    exit 0
  fi
fi
printf 'FAIL: %s requires supported Go (1.26.10 / 1.27.3 or later); found %s\n' \
  "$1" "${version:-unreadable build metadata}" >&2
exit 1
