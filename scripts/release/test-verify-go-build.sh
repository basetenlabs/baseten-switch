#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
cat >"$scratch/go" <<'GO'
#!/usr/bin/env bash
[[ "$1" == version && "$2" == -m ]] || exit 1
printf '%s: %s\n' "$3" "$TEST_GO_VERSION"
GO
chmod +x "$scratch/go"
for version in go1.26.10 go1.26.11 go1.27.3 go1.28.0; do
  TEST_GO_VERSION="$version" GO="$scratch/go" \
    "$SCRIPT_DIR/verify-go-build.sh" fixture >/dev/null
done
for version in go1.25.15 go1.26.9 go1.27.1 go1.27.2 go1.28rc1 devel unknown ''; do
  if TEST_GO_VERSION="$version" GO="$scratch/go" \
    "$SCRIPT_DIR/verify-go-build.sh" fixture >"$scratch/result" 2>&1; then
    printf 'FAIL: accepted unsupported or unknown compiler %s\n' "$version" >&2
    exit 1
  fi
  grep -q 'requires supported Go' "$scratch/result"
done
# Exercise the real build-info reader as well as the version boundaries.
printf 'package main\nfunc main() {}\n' >"$scratch/main.go"
"${GO:-go}" build -o "$scratch/control" "$scratch/main.go"
"$SCRIPT_DIR/verify-go-build.sh" "$scratch/control"
printf 'not an executable\n' >"$scratch/invalid"
if "$SCRIPT_DIR/verify-go-build.sh" "$scratch/invalid" >/dev/null 2>&1; then
  printf 'FAIL: accepted missing build metadata\n' >&2
  exit 1
fi
printf 'PASS: compiler metadata acceptance and rejection\n'
