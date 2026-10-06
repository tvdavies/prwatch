#!/usr/bin/env bash
# Publishes the packages built by npm-packages.mjs, platform packages first.
# Already-published versions are skipped, so a failed release can be re-run.
# In CI this authenticates with npm trusted publishing (OIDC); no token.
set -euo pipefail
out="${1:-npm/dist}"
while read -r dir; do
  [ -n "$dir" ] || continue
  name=$(node -p "require('./$out/$dir/package.json').name")
  version=$(node -p "require('./$out/$dir/package.json').version")
  if npm view "$name@$version" version >/dev/null 2>&1; then
    echo "skip $name@$version (already published)"
    continue
  fi
  echo "publish $name@$version"
  (cd "$out/$dir" && npm publish --provenance --access public)
done < "$out/publish-order.txt"
