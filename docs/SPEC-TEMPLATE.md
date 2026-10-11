# How to write a spec

A spec defines a protocol rule and the packages that implement it. It says
which layer owns the rule, what the rule costs, and which test proves it.
`go test ./internal/specs` checks every `docs/**/SPEC.md` against this
document.

## 1. When to write a spec

Write a spec for behavior that a package under `core/` or `sdk/` implements.

An app that uses only `sdk/` needs no spec in this repository. It documents
itself in its own README.

## 2. The header

The header is a list that follows the title. Each item is required unless
the table says otherwise. `go test ./internal/specs` parses these items.

```markdown
# <Title>

- Status: built
- Layers: core, adapters
- Owns: core/keys, core/store
- Proof: go test -count=1 -run '^TestDelivery$' ./internal/proof/
- Unverified: No test runs a transport on a radio.
```

| Item | Values | Rule |
| --- | --- | --- |
| `Status` | `proposed`, `partial`, `built` | A `proposed` document lives under `docs/proposals/`, see section 4. A `SPEC.md` is `partial` or `built`. |
| `Layers` | `core`, `sdk`, `adapters`, `runtime`, `app` | The layers that the spec changes. A spec that touches only `app` is refused: an app documents itself. |
| `Owns` | Package paths, or `none` | The packages under `core/` and `sdk/` whose behavior this document defines. A spec that owns a `core/` package must list `core` in `Layers`. The same rule holds for `sdk`. |
| `Proof` | One `go test` command | Required for `partial` and `built`. Its `-run` pattern must match at least one test in the named packages. |
| `Remaining` | Text that names a section or holds a link | Required for `partial` only: the acceptance criteria that are not met, by name. An open-ended `partial` is refused. |
| `Unverified` | Text | Required for `partial` and `built`: what no test covers. |

Each package under `core/` and `sdk/` belongs to one spec, through its
`Owns` item. [GRANDFATHERED.md](GRANDFATHERED.md) lists the packages that no
spec owns yet. Do not add a package to that list.

## 3. A change to a built spec

A change to a normative section of a `built` spec is a protocol change. The
change states how an older client behaves. It ships as a breaking commit,
with a `BREAKING CHANGE` footer. If it adds or changes a kind, it updates
[KINDS.md](KINDS.md).

## 4. From a proposal to a spec

A new design starts as a proposal: `docs/proposals/<name>.md`, with the line
`Status: proposed`. The proposal needs no other header item.

When the work is built or built in part, the document moves to
`docs/<name>/SPEC.md` and gets the full header.

## 5. The body

Each `SPEC.md` has exactly these sections, in this order, after its header.
A section can have numbered subsections, such as `### 3.1 Frames`.
`go test ./internal/specs` checks the headings.

| Section | What it holds |
| --- | --- |
| `## 1. Purpose` | What the spec defines, for whom, and what it does not do. What fails without it, and what it replaces. |
| `## 2. Terms` | Each term that the spec defines. A term that another spec owns links to that spec. |
| `## 3. Rules` | The normative rules: wire formats, kinds, tags, fields, limits and states. Write each rule with MUST or MUST NOT. |
| `## 4. Behavior` | How the parties follow the rules, step by step. |
| `## 5. Failures` | Each failure: its cause, what each party does, and what the user sees. |
| `## 6. Security` | What a hostile relay, peer, provider or observer can do, and who pays for storage, CPU and bandwidth. |
| `## 7. Compatibility` | How an older client behaves, what the spec replaced, and how it can be removed. |
| `## 8. Proof` | The tests that prove the rules, the test vectors, and what stays unverified. |

A spec holds rules and their reasons. It does not hold the history of the
work, a list of pull requests, or the guide of one app. The history goes in
the CHANGELOG. An app documents itself in its README, and links to its
`manifest.json`; a spec does not copy a manifest.

A design with nothing built goes in `docs/proposals/`. A `partial` spec keeps
its parts that are not built, starts each of them with "Not built.", and names
them in its `Remaining` item.
