# Packages without a spec

This list is debt. No spec defines the behavior of these packages under
`sdk/`. `go test ./internal/specs` reads the table below.

Do not add a row. To remove a row, name the package in the `Owns` item of a
spec. The spec text must define the behavior of the package. If no spec
defines it, write one.

## Packages without a spec

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
