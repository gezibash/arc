Status: proposed

# Small functions and small packages

No code implements this proposal. It continues
[the programs and the file rule](../PROGRAMS.md). Each claim about ARC names a
file at commit `4b36835`. Each number has a tag:

- *measured*: counted on this commit. Section 8 gives the commands.
- *estimated*: a judgement. No count supports it.

## 0. Bad news first

- The file rule split the god files, but not the god packages. The code of
  `runtime/iface` is 6,115 lines in 22 files, and the code of `cmd/arc` is
  7,131 lines in 37 files (measured, without tests). Each one still does
  many jobs.
- 32 functions are over 100 lines (measured). The longest, `serve` in
  `cmd/arc/serve.go`, is 281 lines.
- `cmd/arc` does work that belongs to the runtime. `serve` hosts an app,
  `bunker.go` and `devices.go` guard a signer, and `message.go` watches mail.
  The arc program cannot share that work, and a test of it must run the CLI.
- Each step below moves code. A move can break a test that reads an
  unexported name. It changes no event and no wire format.

## 1. Summary

1. **The function rule.** No function holds more than 100 lines.
   `TestFunctionSize` in `internal/architecture` checks it. The 32 functions
   that are over today go on a list of debt. The list can only get shorter.
2. **Split `runtime/iface` by job.** The templates, the manifest and the
   arguments become packages of their own. `runtime/iface` keeps the run of
   a command. Whether the line pipeline and the notebook can leave too is
   open; it depends on where the `entry` and `Record` types go.
3. **Thin `cmd/arc`.** Each command reads its flags, opens the session,
   calls one runtime function and prints the result. The host of an app, the
   guard of the bunker, the watch of mail and the folder of the home move to
   `runtime/`.

The order is 1, then 2, then 3. Steps 2 and 3 empty the list of step 1.

## 2. The function rule

- A function, with its signature, holds at most 100 lines. A function
  literal counts in the function that holds it, so a cobra constructor with
  long `RunE` literals counts as one long function.
- `TestFunctionSize` parses each source file with `go/ast`, in the same
  folders as `TestFileSize`. It skips `_test.go` files and `testdata`.
- The debt list names each function that is over today, with its length
  today, as the test counts it with `go/ast`, not as the awk command in
  section 8 counts it. The test fails when a listed function grows, when
  a function that is not listed goes over 100, and when a listed function
  is at or under 100 and stays on the list. So a split must also shorten
  the list.
- AGENTS.md gets one line, beside the file rule.

The 10 longest functions today (measured):

| Lines | Function |
| ---: | --- |
| 281 | `serve`, `cmd/arc/serve.go` |
| 253 | `Session.SyncStorage`, `runtime/citizen/storagesync.go` |
| 221 | `keysCommand`, `cmd/arc/keys.go` |
| 196 | `run.publishSealed`, `runtime/iface/publish_sealed.go` |
| 187 | `messageCommand`, `cmd/arc/message_command.go` |
| 184 | `Manifest.checkCommand`, `runtime/iface/manifest_check.go` |
| 182 | `run.plan`, `runtime/selfsync/repair_plan.go` |
| 180 | `run.notebookAction`, `runtime/iface/notebook.go` |
| 177 | `syncCommand`, `cmd/arc/sync.go` |
| 165 | `server.HandleSession`, `apps/exec/server/session.go` |

Of the 32, 11 are in `cmd/arc`, 6 in `runtime/iface`, and 3 in
`runtime/selfsync` (measured). The other 12 are one in each of 12 packages.

## 3. Split `runtime/iface`

`runtime/iface` runs the commands of a manifest app. Its callers use about
15 names: `Run`, `Parse`, `Installed`, `CallResult`, `Stdio`, `ExitError`
and a few more (measured, in `cmd/arc`, `runtime/citizen`,
`runtime/capability` and `runtime/catalog`). Inside, one `run` struct ties
every file together, so each part can reach each other part.

Today only some files can leave without the `run` struct (measured,
`grep -c 'func (r \*run)'` on each file):

| Part | Files today | Lines | Methods of `run` | Can it leave today? |
| --- | --- | ---: | ---: | --- |
| The manifest: types, parse, check, consent | `manifest.go`, `manifest_check.go`, `consent.go` | 903 | 0 | Yes. No effects. |
| The arguments: bind words to values | `args.go` | 380 | 0 | Yes. It needs only a `Resolver`. |
| The templates: compile and render | `template.go` | 330 | 1 | Yes, without `sanitize`, which uses `run.told`. `sanitize` stays. |
| The line pipeline: select, sort, rank, thread | `pipeline.go` | 550 | 10 | No. 10 functions are methods of `run`, and the pure ones (`field`, `latest`, `sortEntries`, `thread`, `rank`) take `entry` or `Record`, which the runner defines. |
| The notebook: pages and entries | `notebook*.go`, `page_*.go`, `entries.go`, `frontmatter.go` | 1,862 | 30 | No. See below. |
| The runner: query, publish, remove, print | `run.go`, `data.go`, `query.go`, `publish*.go`, `remove.go`, `output.go`, `watch.go` | 2,090 | most | It is `runtime/iface`. |

The planned packages are `runtime/iface/template`, `runtime/iface/manifest`
and `runtime/iface/args`, in that order. Each one imports only the ones
before it: the manifest check calls `compile` of the templates 7 times, and
the arguments bind to the `Command` of the manifest (measured). The line
counts are measured.

- The pipeline and the notebook share one question. Both use the `entry`
  type of `data.go` and the `Record` type of `run.go`.
- The notebook does not use only effects. It calls 10 helpers of the
  runner: `kindOf`, `joined`, `scopedWith`, `selfFilter`, `current`,
  `keyed`, `template`, `opener`, `fetchSelf` and `entryOf` (measured). They
  work on `entry`. So an interface of effects alone cannot replace `run`
  for the notebook.
- The pipeline and the notebook stay in `runtime/iface`, and only their
  long functions are split. A leaf package for `entry` and `Record` could
  take only the functions that are not methods of `run`. The notebook
  files hold 30 methods of `run` and 19 other functions; `pipeline.go`
  holds 10 and 11 (measured, `grep -c` on `^func (r \*run)` and on
  `^func [a-zA-Z]`). So the 40 methods, and the notebook with them, would
  stay behind.
- The templates and the manifest have no effects. Their tests need no
  store. The arguments need a `Resolver`, which can fetch an event.
- A move shortens no function. So each PR that moves a part also splits
  the long functions of that part.
- Each step is one PR that moves one package. Callers keep the names that
  `runtime/iface` gives them today, through type aliases only while the PR
  is open; the PR changes each caller and removes the alias.

## 4. Thin `cmd/arc`

A command in `cmd/arc` does four things: read flags, open the session, call
one runtime function, and print. The work moves as follows:

| Work in `cmd/arc` today | Moves to |
| --- | --- |
| `serve.go`, `announce.go`, `servelock.go`: host an app, announce it, watch mail and sync | `runtime/host` |
| `bunker.go`, the guard and device watch of `devices.go` | `runtime/citizen` (the bunker) |
| `watchMessages` and `sinceFilter` in `message.go` | `runtime/citizen` (the inbox) |
| `home.go`, `path.go`: find the home and the folder of an identity | `runtime/citizen` (the home) |
| `update.go`, `update_pipe.go`: choose and apply a release | `runtime/release` |
| `keysCommand`, `messageCommand`, `syncCommand`, `relayCommand`, `listsCommand` | Stay, split into one function for each subcommand |

- `runtime/host` is new. It hosts one app for months, with the signer of
  its citizen. It is not a program of its own: by the program rule it runs
  on the citizen's machine, with the citizen's key.
- After this step, `cmd/arc` holds about 3,000 lines (estimated), and no
  function in it is over 60 lines (estimated).
- The proofs run the CLI, so they keep checking each command end to end.
  The moved code gains tests that call the runtime without the CLI.

## 5. Order and PRs

| PR | Change | Removes from the debt list |
| --- | --- | ---: |
| 1 | `TestFunctionSize` and the debt list of 32 | 0 |
| 2 | Split the 7 long cobra constructors of `cmd/arc`, `cmd/arc-signer` and `cmd/arc-relay` into one function for each subcommand | 7 |
| 3 | `template`, without `sanitize` | 0 |
| 4 | `manifest`; split `check` and `checkCommand` | 2 |
| 5 | `args`; split `bind` | 1 |
| 6 | split `notebookAction` and `writePage` in place | 2 |
| 7 | `runtime/host`; split `serve` | 1 |
| 8–11 | The bunker, the inbox and the home into `runtime/citizen`; the update into `runtime/release`; split `serveBunker`, `watchMessages` and `update` | 3 |
| 12 | The other 16, one function at a time | 16 |

The counts name the functions on the list today (measured). They add up
to 32.

Each PR passes `go test ./...`, `mise run lint`, `mise run boundaries` and
`mise run specs`. A PR that moves goroutines or process lifetimes, such as
PR 7, gets the label `race`.

## 6. What this does not do

- No package gets a line limit. A package is too large when it does more
  than one job, and a number cannot see that. Sections 3 and 4 name the
  jobs.
- `runtime/selfsync` (3,555 lines) and `core/mail` (2,092 lines) stay as
  they are. Each does one job. Their long functions go off the list in
  PR 12.
- No interface is added for a pure function.

## 7. Risks

- A move of `runtime/iface` touches the app runtime of each manifest app.
  The proofs of the journal and the data apps check it. A behavior change
  in a move is a bug of that PR.
- Type aliases during a move can stay by mistake. PR review checks that
  each PR removes the aliases that it adds.
- A limit of 100 lines can push code into many small functions with long
  argument lists. Review rejects a split that only moves the length into
  parameters.

## 8. Proof and sources

The numbers come from these commands at `4b36835`:

```bash
# Source lines and files of a package, without tests
ls runtime/iface/*.go | grep -v _test.go | xargs cat | wc -l
# Functions over 100 lines, without tests and testdata
find core sdk adapters runtime apps cmd internal -name '*.go' ! -name '*_test.go' ! -path '*/testdata/*' \
  | xargs awk 'FNR==1{f=""} /^func /{f=$0; s=FNR} /^}/{ if(f!=""){ n=FNR-s+1; if(n>100) print n, FILENAME":"s; f=""} }' \
  | sort -rn
```

`TestFunctionSize` replaces the second command when PR 1 merges.

## 9. Unverified

- The size of `cmd/arc` after step 3, and the time that each PR takes, are
  estimates.
- Whether the pipeline and the notebook can leave `runtime/iface` is open.
  PR 6 measures the graph first.
- The counts of methods of `run` are measured. Whether a leaf package for
  `entry` and `Record` breaks the knot of the pipeline and the notebook is
  not: PR 6 finds out.
