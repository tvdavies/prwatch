#!/usr/bin/env bash
# One-time npm set-up for prwatch. Run by the npm package owner, locally.
#
# npm trusted publishing can only be configured for a package that already
# exists, so this publishes a placeholder 0.0.0-bootstrap.0 of each package
# (under the "bootstrap" dist-tag), registers tvdavies/prwatch's
# release.yml as the trusted publisher with direct publish allowed, and
# deprecates the placeholder. Re-running skips steps that are already done.
set -euo pipefail

repo="tvdavies/prwatch"
workflow="release.yml"
placeholder="0.0.0-bootstrap.0"
names=(
  "@tvdavies/prwatch-linux-x64"
  "@tvdavies/prwatch-linux-arm64"
  "@tvdavies/prwatch-darwin-x64"
  "@tvdavies/prwatch-darwin-arm64"
  "prwatch"
)

user=$(npm whoami) || { echo "Run 'npm login' first." >&2; exit 1; }
echo "npm user: $user"
npm_major_minor=$(npm --version | awk -F. '{ printf "%d%03d", $1, $2 }')
if [ "$npm_major_minor" -lt 11015 ]; then
  echo "npm >= 11.15.0 is needed for 'npm trust'; run: npm install -g npm@^11.15.0" >&2
  exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for name in "${names[@]}"; do
  echo "== $name"
  if npm view "$name" name >/dev/null 2>&1; then
    echo "   already on npm"
  else
    dir="$tmp/${name//\//_}"
    mkdir -p "$dir"
    cat >"$dir/package.json" <<JSON
{
  "name": "$name",
  "version": "$placeholder",
  "description": "Placeholder used to set up trusted publishing. See https://github.com/$repo",
  "license": "MIT",
  "repository": { "type": "git", "url": "git+https://github.com/$repo.git" }
}
JSON
    printf '# %s\n\nPlaceholder. See https://github.com/%s\n' "$name" "$repo" >"$dir/README.md"
    (cd "$dir" && npm publish --access public --tag bootstrap)
  fi

  # A package can have only one trust configuration; creating a second
  # fails, which is fine on a re-run. Check with: npm trust list "$name"
  npm trust github "$name" --file "$workflow" --repo "$repo" --allow-publish --yes ||
    echo "   could not add the trusted publisher (already configured?); check: npm trust list $name"

  if npm view "$name@$placeholder" version >/dev/null 2>&1; then
    npm deprecate "$name@$placeholder" "Placeholder used to set up trusted publishing; install a real release." || true
  fi
  sleep 2
done

echo
echo "Done. Check each package's settings on npmjs.com, then consider setting"
echo "'Publishing access' to 'Require two-factor authentication and disallow tokens'."
