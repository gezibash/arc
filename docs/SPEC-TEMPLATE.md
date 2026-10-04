# How to write a spec

A spec protects the protocol and the repository. It records why a change
exists, which layer owns it, what it costs forever, and how a test proves it.
`go test ./internal/specs` checks every `docs/**/SPEC.md` against this
document. The pull request checks in `.github/workflows/pr-policy.yml` check
that the record grows and never shrinks.

## 1. The default answer is no

A spec must overcome the default with facts: a failing test, a named request,
a measurement, or a security finding. An opinion is not a fact. "It is cleaner
in core" is an opinion.

An app that uses only `sdk/` needs no spec in this repository. It documents
itself in its own README, in its own repository.

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
| `Status` | `proposed`, `partial`, `built` | A `proposed` document lives under `docs/proposals/`, see section 6. A `SPEC.md` is `partial` or `built`. |
| `Layers` | `core`, `sdk`, `adapters`, `runtime`, `app` | The layers that the spec changes. A spec that touches only `app` is refused: an app documents itself. |
| `Owns` | Package paths, or `none` | The packages under `core/` and `sdk/` whose behavior this document defines. A spec that owns a `core/` package must list `core` in `Layers`. The same rule holds for `sdk`. |
| `Proof` | One `go test` command | Required for `partial` and `built`. Its `-run` pattern must match at least one test in the named packages. |
| `Remaining` | Text that names a section or holds a link | Required for `partial` only: the acceptance criteria that are not met, by name. An open-ended `partial` is refused. |
| `Unverified` | Text | Required for `partial` and `built`: what no test covers. |

## 3. The gates

The gates are questions. A human answers them before anyone writes design
text. An agent records the answers verbatim; it never answers for the human.

Which gates a spec must answer depends on its `Layers`:

| Layers | Required gates |
| --- | --- |
| `adapters`, `runtime` | A1, A2, A3, B1, B3, B4, D1, D2, D3, D4 |
| `sdk` | The rows above, and C1, C2, C3, C4, C5 |
| `core` | The rows above, and B2, D5 |

### A. Does this belong in this repository?

- **A1.** What fails today without this?
- **A2.** Can it be an app in another repository that uses only `sdk/`?
- **A3.** What does it remove or replace?

### B. Which layers, and why not the one above?

- **B1.** Which layers does it touch? For each item in `core/` or `sdk/`,
  why can it not live one layer up?
- **B2.** Which wire elements does it add or change: event kinds, tags,
  frames, fields, signatures?
- **B3.** Does it touch a network, a file, a process or a database?
- **B4.** Which terms does it define, and which does it borrow from other
  specs?

### C. What does it cost forever?

- **C1.** What can never be removed once shipped?
- **C2.** What breaks for an older client, an older manifest, or stored
  state? What does an old client do: ignore, fail closed, or break?
- **C3.** What can a hostile peer, a hostile relay, or an attacker do that
  they could not do before? Who pays the cost: relay storage, peer CPU, or
  citizen bandwidth?
- **C4.** How does it die? What does deprecation look like, and what stored
  state must migrate?
- **C5.** What is the smallest useful version, and what is out of scope?

### D. How will we know it works?

- **D1.** Which test proves it through the normal CLI and a real transport?
- **D2.** Which existing tests must change?
- **D3.** How does it fail, and what does the user see?
- **D4.** What stays unverified?
- **D5.** Could a second implementation be written from this document alone?
  Which test vectors are checked in?

The question IDs never change. A new question gets a new ID. A retired
question stays in this list, marked retired, so old records still read.

## 4. The record

The answers live in the spec, in a section named `## Gates`. One subsection
per question, in this exact form:

```markdown
## Gates

### A1. What fails today without this?

> The answer, in the words of the person who gave it.

Answered by Ada Lovelace on 2026-09-30 at 1a2b3c4.
Evidence: `go test -run '^TestX$' ./core/keys/` fails at 1a2b3c4.
```

Rules:

- The quote is verbatim. An agent does not paraphrase, shorten or improve it.
- The attribution names the person, the date, and the commit at which the
  answer was true. Facts such as "test X fails" are checkable only at that
  commit.
- `Evidence` names something a reviewer can check: a test that exists, a
  link, a commit hash, or a measurement with its command.
- `N/A`, `TBD`, `TODO`, a bare `yes` or `no`, and an answer shorter than eight
  words are not answers.
- The section is append-only. To change an answer, add a dated line under the
  question: `Revised on 2026-10-04 by Ada Lovelace at 9f1e2c3: ...`. Never
  edit or delete an earlier line.

The record is not a verdict. It lets a reviewer of the spec, or of the code
built on it, see what was asked, what was answered, and with what evidence.

## 5. A change to a built spec

A change to a normative section of a `built` spec is a protocol change. It
must answer gates C1 to C4 again, in dated revisions, and state how an older
client behaves. It ships as a breaking commit, with a `BREAKING CHANGE`
footer, and it updates [KINDS.md](KINDS.md) if it adds or changes a kind.

## 6. From a proposal to a spec

A new spec starts as a proposal: `docs/proposals/<name>.md`, with the line
`Status: proposed` and its `## Gates` section. The record can be incomplete
there, and the document needs no other header item.

When each required gate has its answer, and the work is built or built in
part, the document moves to `docs/<name>/SPEC.md` and gets the full header.
The record moves with it. The record is append-only in a proposal too.

## 7. Grandfathered specs

[GRANDFATHERED.md](GRANDFATHERED.md) lists the specs that predate the gates,
and the packages that no spec owns. The list only shrinks. An entry leaves it
when a human answers the gates, or when a spec claims the package.

## 8. A pull request

The check "Spec policy" reads each pull request. It refuses three things:

- A change that edits or deletes a line of a `## Gates` section. A pull
  request can add lines, and it can delete a whole document.
- A change that adds a row to [GRANDFATHERED.md](GRANDFATHERED.md).
- A change to a package under `core/` or `sdk/` whose description does not
  name the spec that owns the package. Write one line for each spec:
  `Spec: docs/<name>/SPEC.md`. A package on the grandfathered list of the
  base branch needs no line. A test, its data, and a `doc.go` need no line; a
  `doc.go` holds only the package comment.

The check runs the rules of the base branch, so a pull request cannot change
the rules that judge it.

## 9. The body

Each `SPEC.md` has exactly these sections, in this order, after its header.
A section can have numbered subsections, such as `### 3.1 Frames`.
`go test ./internal/specs` checks the headings.

| Section | What it holds | Gates |
| --- | --- | --- |
| `## 1. Purpose` | What the spec defines, for whom, and what it does not do. | A1, A3, C5 |
| `## 2. Terms` | Each term that the spec defines. A term that another spec owns links to that spec. | B4 |
| `## 3. Rules` | The normative rules: wire formats, kinds, tags, fields, limits and states. Write each rule with MUST or MUST NOT. | B1, B2, C1 |
| `## 4. Behavior` | How the parties follow the rules, step by step. | B3 |
| `## 5. Failures` | Each failure: its cause, what each party does, and what the user sees. | D3 |
| `## 6. Security` | What a hostile relay, peer, provider or observer can do, and who pays for storage, CPU and bandwidth. | C3 |
| `## 7. Compatibility` | How an older client behaves, what the spec replaced, and how it can be removed. | C2, C4 |
| `## 8. Proof` | The tests that prove the rules, the test vectors, and what stays unverified. | D1, D2, D4, D5 |
| `## Gates` | The record of the gates, see section 4. | |

A spec holds rules and their reasons. It does not hold the history of the
work, a list of pull requests, or the guide of one app. The history goes in
the CHANGELOG. An app documents itself in its README, and links to its
`manifest.json`; a spec does not copy a manifest.

A design with nothing built goes in `docs/proposals/`. A `partial` spec keeps
its parts that are not built, starts each of them with "Not built.", and names
them in its `Remaining` item.
