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
2. **Split `runtime/iface` by job.** The manifest, the arguments, the
   templates, the line pipeline and the notebook become packages of their
   own. `runtime/iface` keeps the run of a command.
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
  today. The test fails when a listed function grows, when a function that
  is not listed goes over 100, and when a listed function is at or under 100
  and stays on the list. So a split must also shorten the list.
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

| New package | Files today | Lines | Job | Effects |
| --- | --- | ---: | --- | --- |
| `runtime/iface/manifest` | `manifest.go`, `manifest_check.go`, `consent.go` | 903 | The manifest types, parse and check, consent | None |
| `runtime/iface/template` | `template.go` | 330 | Compile and render a template | None |
| `runtime/iface/args` | `args.go` | 380 | Bind the words of a command to values | A `Resolver` |
| `runtime/iface/pipeline` | `pipeline.go` | 550 | Select, sort, rank and print lines | None |
| `runtime/iface/notebook` | `notebook*.go`, `page_*.go`, `entries.go`, `frontmatter.go` | 1,862 | The pages and entries of a notebook | A narrow `Pages` store |
| `runtime/iface` | `run.go`, `data.go`, `query.go`, `publish*.go`, `remove.go`, `output.go`, `watch.go` | 2,090 | Run one command: query, publish, remove, print | `Env` |

The line counts are measured. The dependency order is `manifest`, then
`template`, `args` and `pipeline`, then `notebook`, then `iface`. No package
imports one that comes after it.

- The notebook is the second largest part. 30 of its functions are
  methods of `run` (measured). They use mostly the environment, the
  clock, the flags and the output.
  A `Pages` interface with those effects replaces `run` for it. The
  interface lives in `runtime/iface/notebook`, the layer that consumes it.
- `manifest`, `template` and `pipeline` have no effects. Their tests need
  no store.
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
| 3–7 | One PR for each new package of `runtime/iface` | 6 |
| 8 | `runtime/host` | 1 |
| 9–11 | The bunker, the inbox and the home into `runtime/citizen`; the update into `runtime/release` | 4 (estimated) |
| 12 | The rest of the list, one function at a time | The rest |

Each PR passes `go test ./...`, `mise run lint`, `mise run boundaries` and
`mise run specs`. A PR that moves goroutines or process lifetimes, such as
PR 8, gets the label `race`.

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
- No count shows that the `Pages` interface of the notebook is small enough.
  The PR that moves the notebook measures it first.
