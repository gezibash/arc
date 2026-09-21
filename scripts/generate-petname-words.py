#!/usr/bin/env python3
"""Generate the Go petname wordlists from the Elixir source.

    python3 scripts/generate-petname-words.py

The two implementations must hold the same words in the same order. A petname
comes from an index into these lists, so one different word gives one
different name for the same key.
"""

import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parent.parent
SOURCE = ROOT / "apps/arc_identity/lib/arc/identity/petname.ex"
TARGET = ROOT / "go/identity/petname_words.go"


def words(source: str, name: str) -> list[str]:
    block = re.search(r"@%s_list ~w\((.*?)\)" % name, source, re.S)
    if block is None:
        raise SystemExit(f"{SOURCE}: no list named {name}")
    return block.group(1).split()


def render(name: str, items: list[str]) -> str:
    lines = [
        "\t" + " ".join('"%s",' % word for word in items[index : index + 8])
        for index in range(0, len(items), 8)
    ]
    return "var %s = [256]string{\n%s\n}\n" % (name, "\n".join(lines))


def main() -> None:
    source = SOURCE.read_text()
    adjectives = words(source, "adjectives")
    nouns = words(source, "nouns")

    for name, items in (("adjectives", adjectives), ("nouns", nouns)):
        if len(items) != 256:
            raise SystemExit(f"{SOURCE}: {name} holds {len(items)} words, not 256")

    TARGET.write_text(
        "// Code generated from apps/arc_identity/lib/arc/identity/petname.ex.\n"
        "// Run: python3 scripts/generate-petname-words.py\n\n"
        "package identity\n\n" + render("adjectives", adjectives) + "\n" + render("nouns", nouns)
    )
    print(f"wrote {TARGET}")


if __name__ == "__main__":
    main()
