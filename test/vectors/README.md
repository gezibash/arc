# The shared protocol vectors

`identity.json` holds values that every ARC implementation must produce:
identities and the keys that follow from a seed, HKDF outputs, one sealed
box, one signed announcement, three signed capability packages, one session
with one packet, and one signed release channel.

The Elixir implementation wrote this file. Go read it, and the two agreed.
That is what the file records. The Elixir tree is gone, so nothing generates
the file any more. It is a fixture now.

A change to a vector is a change to the protocol. To add one, write it in Go
and say in the commit what it pins.

Two fields carry fresh randomness on each write, and therefore never
compared across runs: `sealed_box` and `session`.

Each path under `packages` starts at the root of the repository.
