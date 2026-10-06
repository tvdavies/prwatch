# Releasing prwatch

A release is a `v*` tag on `main`. The [release workflow](.github/workflows/release.yml) then:

1. runs the tests;
2. runs GoReleaser to create the GitHub release, which holds:
   - `prwatch_<version>_<os>_<arch>.tar.gz` archives;
   - `checksums.txt`;
   - bare `gh-prwatch_<tag>_<os>-<arch>` binaries for the gh extension;
3. builds the five npm packages and publishes them with npm trusted publishing (OIDC). There is no npm token, and provenance is generated automatically;
4. mirrors the `gh-prwatch_*` binaries to `tvdavies/gh-prwatch`, if that is set up.

## One-time set-up

### npm

The npm packages are:

| Package | Contents |
| --- | --- |
| `@tvdavies/prwatch` | launcher and postinstall, installing the `prwatch` command; depends on the four below as optional dependencies |
| `@tvdavies/prwatch-linux-x64` | Linux x86-64 binary |
| `@tvdavies/prwatch-linux-arm64` | Linux arm64 binary |
| `@tvdavies/prwatch-darwin-x64` | macOS Intel binary |
| `@tvdavies/prwatch-darwin-arm64` | macOS Apple silicon binary |

All five packages live under the `@tvdavies` scope, so nobody else can squat them. The main package was going to be the unscoped `prwatch`, but npm rejects that name as too similar to `watch`. The installed command is still `prwatch`.

npm only lets you add a trusted publisher to a package that already exists. Since 3 September 2026, a new trust configuration only allows `npm stage publish` unless direct publish is enabled explicitly. The bootstrap script deals with both.

1. Make sure two-factor authentication is on for the npm account (`tvdavies`), and that you have npm 11.15 or newer:

   ```sh
   npm install -g npm@^11.15.0
   npm login
   ```

2. From a checkout of this repo, run:

   ```sh
   scripts/npm-bootstrap.sh
   ```

   For each of the five packages, it:
   - publishes a placeholder `0.0.0-bootstrap.0` under the `bootstrap` dist-tag, unless the package already exists (`npm view <package> version` succeeds);
   - runs `npm trust github <package> --file release.yml --repo tvdavies/prwatch --allow-publish --yes`, unless `npm trust list <package> --json` already shows `tvdavies/prwatch` and `release.yml`;
   - deprecates the placeholder, unless it is already deprecated.

   The script is safe to re-run: it skips anything that is already done. To check a package by hand, run `npm trust list <package>`.

   npm will ask for 2FA. In the browser prompt, tick the option that skips 2FA for the next five minutes so that the remaining packages go through.

   To do it by hand instead, run these for each package once it exists:

   ```sh
   npm trust github <package> --file release.yml --repo tvdavies/prwatch --allow-publish --yes
   ```

   Or use the npm website: go to the package, then Settings, then Trusted publishing, and add GitHub Actions. Set:
   - organisation or user: `tvdavies`;
   - repository: `prwatch`;
   - workflow filename: `release.yml`;
   - environment: leave blank.

   Allow `npm publish`.

3. Optional but recommended: in each package's settings, set publishing access to "Require two-factor authentication and disallow tokens". Trusted publishing keeps working with this setting.

### gh extension

`gh extension install` only accepts repositories whose name starts with `gh-` (see `checkValidExtension` in cli/cli). So `gh extension install tvdavies/prwatch` cannot work while the repo is named `prwatch`. Instead, the release workflow mirrors the extension binaries to a companion repo:

1. Create a public repo `tvdavies/gh-prwatch` with a README. It needs at least one commit, so that releases can be tagged.
2. Create a fine-grained personal access token with:
   - repository access: only `tvdavies/gh-prwatch`;
   - permissions: Contents read and write.
3. Add it to `tvdavies/prwatch` as an Actions secret named `GH_EXTENSION_TOKEN`.

Until the secret exists, the mirror job logs a skip and succeeds.

Users then install with `gh extension install tvdavies/gh-prwatch` and run `gh prwatch …`. gh picks the release asset whose name ends in `<os>-<arch>`, which is the gh-extension-precompile convention.

## Cutting a release

1. Make sure `main` is green.
2. Tag and push:

   ```sh
   git checkout main && git pull
   git tag -a v0.1.0 -m "prwatch 0.1.0"
   git push origin v0.1.0
   ```

3. Watch the run with `gh run watch --repo tvdavies/prwatch`.
4. Check the results:

   ```sh
   npm view @tvdavies/prwatch version
   npm view @tvdavies/prwatch dist.attestations   # provenance
   npm i -g @tvdavies/prwatch && prwatch daemon restart && prwatch version && file -L "$(command -v prwatch)"   # should be a native binary
   gh release view v0.1.0 --repo tvdavies/prwatch
   gh extension install tvdavies/gh-prwatch && gh prwatch version
   ```

The publish script skips versions that are already on npm. If a release fails part-way, re-run the failed job.

## Version numbers

The version comes from the tag. GoReleaser injects it into the binary (`prwatch version`), and the npm packages take it from the tag with the `v` removed.
