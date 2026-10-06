# prwatch

One shared GitHub pull request poller per user per machine, with a blocking `wait` command for agents and scripts.

```sh
prwatch wait owner/repo#123 --for checks   # blocks until CI settles, then prints the PR
```

## Why

AI agents, scripts and terminal sessions that babysit PRs each poll GitHub on their own. A handful of agents polling a handful of PRs every few seconds exhausts a user's API budget quickly. It usually hits GitHub's secondary GraphQL limits first, and GitHub warns that carrying on while rate-limited can get an integration banned.

prwatch replaces all of that polling with one poller:

- every `prwatch wait` connects to a single per-user daemon;
- the daemon polls the union of watched PRs with **one batched GraphQL request per round**, at about 1 point;
- it backs off strictly when GitHub says so.

The daemon is written in Go, so each blocked waiter is a small static process (about MEM_WAITER of RSS) rather than a 75 MB Node process.

## Install

Pick one:

```sh
npm i -g @tvdavies/prwatch               # installs the native binary for your platform as `prwatch`
npx @tvdavies/prwatch status owner/repo#1 # one-off use (see the note on memory below)
gh extension install tvdavies/gh-prwatch  # then run: gh prwatch …
go install github.com/tvdavies/prwatch@latest
```

The npm package is scoped (`@tvdavies/prwatch`), but the command it installs is `prwatch`.

Release binaries for Linux and macOS (amd64 and arm64) are on the [releases page](https://github.com/tvdavies/prwatch/releases). Windows isn't supported yet.

Authentication uses the first of these that is set: `GH_TOKEN`, `GITHUB_TOKEN`, or `gh auth token`. Tokens are never logged.

## Commands

A `<pr>` can be:
- `owner/repo#123`;
- a PR URL;
- `123` or `'#123'` inside a checkout whose `origin` remote is on GitHub. Quote `#123` in shells.

| Command | What it does |
| --- | --- |
| `prwatch wait <pr> [--for COND] [--since TOKEN] [--timeout D] [--json]` | Block until the condition holds, then print the snapshot and its token. |
| `prwatch status <pr...> [--json]` | Print snapshots: from the daemon's cache if it is running, otherwise from one direct batched request. Never starts the daemon. |
| `prwatch events [--pr <pr>]... [--json]` | Stream one line per change (JSON Lines with `--json`). Without `--pr`, streams every watched PR. Counts as a waiter. |
| `prwatch list [--json]` | Show what the daemon is watching, who is waiting and the budget. Never starts or keeps alive the daemon. |
| `prwatch rate [--json]` | Show the remaining budget, last cost, poll interval and any back-off. |
| `prwatch daemon status` / `prwatch daemon stop` | Inspect or stop the daemon. |
| `prwatch version` | Print the version. |

### `--for` conditions

| Condition | Met when |
| --- | --- |
| `change` (default) | any material field changes (see [Snapshot](#snapshot-json)) |
| `checks` | the check rollup has left `PENDING`/`EXPECTED`. A head commit with no checks counts as settled. |
| `review` | a new review, a reply in a review thread, a thread is opened or resolved, or a review or thread comment is [edited](#edits) |
| `mergeable` | open and not a draft, approved (or no review required), checks green, no unresolved threads, and mergeable |
| `merged` | the PR is merged |
| `closed` | the PR is closed or merged |

`change` and `review` compare against the first snapshot `wait` sees. The others are level-triggered: if the condition already holds, `wait` returns at once.

`--since TOKEN` makes `wait` report only a state that differs from `TOKEN`. If the PR has already moved on, `wait` returns straight away. `change` and `review` compare against the token rather than the first snapshot. This is how an agent loops without missing anything that happens between calls.

Conflicts and base-branch moves are detected even when nothing on the PR itself changes, because `mergeable` and `mergeStateStatus` are part of the state (a PR that falls `BEHIND` a base requiring up-to-date branches wakes `change` too). GitHub computes mergeability lazily, so the first poll after the base moves may return `UNKNOWN`. That wakes `change` once; prwatch then polls again quickly, and the next state, `CONFLICTING` with the `conflict` reason for example, follows within seconds.

### Edits

Editing the PR description, an issue comment, a review body or a review-thread comment wakes `change` and appears in `events` as `description edited`, `comment edited` or `review edited`. Review and thread-comment edits also wake `review`. To keep each round cheap, only edits within the fetched window are seen: the newest 3 issue comments, the latest review from each reviewer, and the last 10 comments of each of the newest 5 review threads. Edits to older comments or threads are not seen.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | condition met (or command succeeded) |
| 124 | `--timeout` elapsed; the latest snapshot is still printed |
| 2 | usage or authentication error, including a PR GitHub refuses to show (`FORBIDDEN`, SAML) |
| 3 | PR or repository not found |
| 1 | anything else, such as a temporary GitHub error, a back-off, an [incomplete](#snapshot-json) snapshot from `status`, or the daemon being stopped while you wait |

## Snapshot JSON

`--json` prints a versioned snapshot. Fields are only ever added within a schema version.

```json
{
  "schemaVersion": 1,
  "pr": "owner/repo#123",
  "owner": "owner", "repo": "repo", "number": 123,
  "url": "https://github.com/owner/repo/pull/123",
  "title": "Add the thing",
  "state": "OPEN",
  "isDraft": false,
  "merged": false, "mergedAt": null, "mergeCommit": null,
  "headRefOid": "4b1c…", "baseRefName": "main",
  "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN",
  "autoMerge": { "enabled": false, "method": null },
  "reviewDecision": "APPROVED",
  "reviews": [{ "author": "alice", "state": "APPROVED", "submittedAt": "…", "commit": "4b1c…", "editedAt": null }],
  "reviewCount": 3,
  "reviewRequests": ["bob", "team:platform"],
  "threads": {
    "total": 4, "unresolved": 1,
    "items": [{ "id": "PRRT_…", "path": "main.go", "line": 42, "outdated": false, "author": "alice", "excerpt": "Could this…" }],
    "edits": { "PRRT_…": "…" }
  },
  "checks": {
    "state": "SUCCESS", "total": 6,
    "contexts": [{ "name": "test", "kind": "check_run", "status": "COMPLETED", "conclusion": "SUCCESS", "required": true }]
  },
  "comments": { "total": 2, "recent": [{ "author": "bob", "createdAt": "…", "excerpt": "LGTM", "editedAt": null }] },
  "updatedAt": "…",
  "bodyEditedAt": null,
  "needsAction": true,
  "reasons": ["unresolved_threads"],
  "token": "1.3fa9c1e2b7d04a11.9a1b2c3d",
  "fetchedAt": "…",
  "incomplete": false
}
```

The values come from GitHub's GraphQL API:
- `state` is `OPEN`, `CLOSED` or `MERGED`.
- `checks.state` is GitHub's rollup state, or `NONE` when the head commit has no checks.
- `checks.contexts[].kind` is `check_run` or `status`. `required` is null if GitHub can't tell.
- `reviewDecision` is null when no review policy applies.
- `reviews` holds the latest review from each author.
- `reviewCount` counts every review, including replies in threads.
- `editedAt` and `bodyEditedAt` are GitHub's `lastEditedAt`, or null if never edited. `threads.edits` maps each of the newest 5 threads with an edited comment to its latest edit (omitted when there are none); an unresolved thread item among them also carries its own `editedAt`.

`incomplete` is true when GitHub returned the PR but an error nulled part of it (its checks, for example). `incompleteReason` then says which field failed and why. An incomplete snapshot is never treated as authoritative:
- it never satisfies a `--for` condition and is never `ready_auto_merge_off`;
- the daemon keeps the previous good snapshot, emits no change event and fetches the PR again next round;
- `status` prints it only when there is no previous good snapshot, and then exits 1.

`needsAction` is true when `reasons` is non-empty. For open PRs only, the reasons are:

| Reason | Meaning |
| --- | --- |
| `required_check_failed` | a required check failed, timed out, was cancelled or needs action |
| `check_failed` | a non-required check failed |
| `changes_requested` | the review decision is `CHANGES_REQUESTED` |
| `unresolved_threads` | at least one review thread is unresolved |
| `conflict` | `mergeable` is `CONFLICTING` |
| `ready_auto_merge_off` | approved, green, no unresolved threads and mergeable, but auto-merge is off |

`token` is `1.<16 hex>.<8 hex>`:
- the first hash covers every material field: state, draft, merge commit, head and base, title, mergeability, auto-merge, review decision, reviews, review requests, threads, checks, comment count and [edits](#edits);
- the second covers reviews and threads only, including review and thread-comment edits.

A PR on which nothing has been edited keeps the same token as in 0.1.0.

`updatedAt`, `fetchedAt` and excerpts are not material.

## How the daemon works

- **Start-up:** `wait`, `events` and `status` connect to a per-user unix socket, `prwatch.sock`. It lives in `$XDG_RUNTIME_DIR/prwatch`, or `~/Library/Caches/prwatch` on macOS, or `~/.cache/prwatch`. The directory is created with mode 0700. If nothing is listening, `wait` and `events` start the daemon: the same executable with a hidden `__daemon` subcommand, in a new session, logging to `daemon.log` in that directory.
- **One daemon:** the daemon holds an exclusive `flock` on `daemon.lock` for its whole life. Clients that start at the same moment therefore end up with exactly one daemon, and a stale socket left by a crash is replaced safely.
- **Interest:** each open connection registers interest in its PRs, using a small JSON-lines protocol. When a waiter exits or is killed, its interest goes with it.
- **Polling:** the daemon polls the union of PRs that open connections care about. It never sends requests concurrently:
  - The first time it sees a PR, one aliased query resolves `owner/repo#n` to a node id and returns the full snapshot. Ids are cached in `ids.json`.
  - After that, each round is one aliased `node(id:)` query for up to 50 PRs, plus `rateLimit { cost remaining resetAt }`.
  - The first comment of each new unresolved review thread costs one small extra request, made only when such threads appear. The result is cached, and fetched again after an edit in the thread.
  - A round of up to 10 PRs costs 1 point. The edit window adds a small nested connection, so bigger rounds cost a little more (about 3 points for 20 PRs, 7 for 50).
- **Exit:** with no waiters left, the daemon exits after an idle grace period (30s by default) and removes the socket.

## Rate limits

- **Adaptive interval:** about 10s while any watched PR has pending checks, auto-merge enabled or unknown mergeability; otherwise about 60s. Rounds never start less than 5s apart, including the fetches for newly watched PRs.
- **Budget guard:** prwatch spends at most `PRWATCH_BUDGET_SHARE` (20% by default) of the primary budget remaining before reset. As the budget shrinks, the interval stretches to match the last round's cost. If less than one round is affordable, it waits for the reset. The budget is persisted to `rate.json`, so a restarted daemon with a low stored budget also waits for the reset.
- **Strict back-off:**
  - it honours `retry-after`;
  - if the primary budget is exhausted, it waits until `x-ratelimit-reset`;
  - for any other rate-limit response it waits at least 60s, doubling on each consecutive hit up to 30 minutes.

  The back-off is persisted to `rate.json`, so a restarted daemon or a direct `status` call honours it too.

Configuration is by environment variable, read when the daemon starts:

| Variable | Default | Meaning |
| --- | --- | --- |
| `PRWATCH_IDLE_GRACE` | `30s` | how long the daemon lingers with no waiters |
| `PRWATCH_BUDGET_SHARE` | `0.2` | share of the remaining primary budget prwatch may spend before reset |
| `PRWATCH_STATE_DIR` | see above | socket, lock, log, `rate.json` and `ids.json` |
| `PRWATCH_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `PRWATCH_POLL_FAST` / `PRWATCH_POLL_SLOW` | `10s` / `60s` | poll intervals (minimum 5s) |

The daemon inherits its environment, including the token, from whichever client started it. Run `prwatch daemon stop` after changing configuration.

## Memory per waiter

MEMORY_TABLE

## Using with AI agents

**Babysit loop.** Pass the previous token back with `--since`. Then nothing that happens between calls is missed, and the loop doesn't spin:

```sh
tok=""
while :; do
  out=$(prwatch wait owner/repo#123 --json --timeout 10m ${tok:+--since "$tok"}) || [ $? -eq 124 ] || break
  tok=$(jq -r .token <<<"$out")
  jq -r '"\(.state) checks=\(.checks.state) review=\(.reviewDecision) reasons=\(.reasons|join(","))"' <<<"$out"
  [ "$(jq -r .state <<<"$out")" = OPEN ] || break
  # act on .needsAction / .reasons here: fix CI, answer threads, enable auto-merge…
done
```

To block until CI settles after a push, use `prwatch wait 123 --for checks`. To block until a PR can merge, use `prwatch wait 123 --for mergeable --timeout 30m`.

**Claude Code's Monitor tool.** `events` prints one line per change and keeps the daemon alive while it runs:

```
Monitor({ command: "prwatch events --pr owner/repo#123 --pr owner/repo#124", description: "PR changes" })
```

**What's being watched.** `prwatch list` shows every watched PR with its waiters and their conditions, how long it has been watched, a one-line state summary and the last poll time. A footer gives the interval, budget and back-off. It never starts the daemon or keeps it alive.

## Development

```sh
go test -race ./...                      # e2e tests run the real CLI and daemon against a fake GraphQL server
PRWATCH_LIVE=1 go test -run Live ./internal/cli   # one real request to api.github.com
```

See [RELEASING.md](RELEASING.md) for releases.

## Licence

MIT
