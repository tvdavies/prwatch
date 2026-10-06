#!/usr/bin/env bash
# One-time npm set-up for prwatch. Run by the npm package owner, locally.
#
# npm trusted publishing can only be configured for a package that already
# exists. For each package this:
#   1. publishes a placeholder 0.0.0-bootstrap.0 (under the "bootstrap"
#      dist-tag), unless the package already exists on npm;
#   2. registers tvdavies/prwatch's release.yml as the trusted publisher with
#      direct publish allowed, unless `npm trust list` already shows it;
#   3. deprecates the placeholder, unless it is already deprecated.
# It is safe to re-run: steps that are already done are skipped.
set -euo pipefail

repo="tvdavies/prwatch"
workflow="release.yml"
placeholder="0.0.0-bootstrap.0"
names=(
  "@tvdavies/prwatch-linux-x64"
  "@tvdavies/prwatch-linux-arm64"
  "@tvdavies/prwatch-darwin-x64"
  "@tvdavies/prwatch-darwin-arm64"
  "@tvdavies/prwatch"
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

# exists NAME: 0 if the package is on npm, 1 if npm says 404. Anything else
# (network, auth) aborts rather than risk a confusing publish attempt.
exists() {
  local out
  if out=$(npm view "$1" version 2>&1); then
    return 0
  fi
  if grep -q E404 <<<"$out"; then
    return 1
  fi
  printf '%s\n' "$out" >&2
  echo "Could not check whether $1 is on npm." >&2
  exit 1
}

# trusted NAME: 0 if the package already trusts release.yml in this repo.
# Returns 2 if the trust configuration could not be read.
trusted() {
  local out
  # stderr stays visible: npm may ask for 2FA here.
  out=$(npm trust list "$1" --json) || return 2
  grep -q "\"repository\": \"$repo\"" <<<"$out" && grep -q "\"file\": \"$workflow\"" <<<"$out"
}

for name in "${names[@]}"; do
  echo "== $name"
  if exists "$name"; then
    echo "   already on npm; not publishing a placeholder"
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
    # The registry can take a moment before the new package accepts settings.
    sleep 5
  fi

  status=0
  trusted "$name" || status=$?
  if [ "$status" -eq 0 ]; then
    echo "   trusted publisher already configured ($repo, $workflow)"
  else
    [ "$status" -eq 2 ] && echo "   could not read the trust configuration; trying to add it anyway"
    # A package can have only one trust configuration, so this fails if a
    # different one exists. Check with: npm trust list $name
    npm trust github "$name" --file "$workflow" --repo "$repo" --allow-publish --yes ||
      echo "   could not add the trusted publisher; check: npm trust list $name"
  fi

  if npm view "$name@$placeholder" version >/dev/null 2>&1; then
    if [ -n "$(npm view "$name@$placeholder" deprecated 2>/dev/null)" ]; then
      echo "   placeholder already deprecated"
    else
      npm deprecate "$name@$placeholder" "Placeholder used to set up trusted publishing; install a real release." || true
    fi
  fi
done

echo
echo "Done. Check each package with: npm trust list <package>"
echo "Then consider setting 'Publishing access' to 'Require two-factor"
echo "authentication and disallow tokens' in each package's settings on npmjs.com."
