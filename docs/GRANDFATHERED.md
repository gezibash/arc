# Grandfathered specs and packages

This list is debt. Each entry predates the rules of
[SPEC-TEMPLATE.md](SPEC-TEMPLATE.md). `go test ./internal/specs` reads the two
tables below.

The list only shrinks. The pull request check refuses a change that adds a
row. To remove a row:

- For a spec, a human answers its gates. The `spec` skill runs the
  interview. Do this before the next change to a normative section of the
  spec.
- For a package, a spec names it in its `Owns` item. The spec text must
  define the behavior of the package. If no spec defines it, write one.

## Specs without gates

Nobody has answered the gates of these specs.

| Spec | Since |
| --- | --- |
| `docs/delivery/SPEC.md` | 2026-09-30 |
| `docs/http/SPEC.md` | 2026-09-30 |
| `docs/interface/SPEC.md` | 2026-09-30 |
| `docs/sessions/SPEC.md` | 2026-09-30 |
| `docs/updates/SPEC.md` | 2026-09-30 |
| `docs/wake/SPEC.md` | 2026-09-30 |

## Packages without a spec

No spec defines the behavior of these packages under `sdk/`.

| Package | Since |
| --- | --- |
| `sdk/atomicfile` | 2026-09-30 |
| `sdk/execadapter` | 2026-09-30 |
| `sdk/limitio` | 2026-09-30 |
| `sdk/ndjson` | 2026-09-30 |
| `sdk/providerconfig` | 2026-09-30 |
| `sdk/providertest` | 2026-09-30 |
| `sdk/stdio` | 2026-09-30 |
| `sdk/strictjson` | 2026-09-30 |
