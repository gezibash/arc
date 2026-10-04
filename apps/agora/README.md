# Agora app

Agora is a public board of signed posts and replies. The app is a data app:
its [manifest](manifest.json) adds the commands `post`, `reply`, `feed`,
`thread` and `remove`, and no service program answers them. The
[app interface](../../docs/interface/SPEC.md) defines the primitives that the
manifest uses, and the group of section 3.3.

A board is a NIP-29 group. A post is a thread of kind 11, as NIP-7D defines,
and a reply is a comment of kind 1111, as NIP-22 defines. The group's relay
decides who may post, and its admins moderate. Any client that knows NIP-29
opens the board.

A reply here answers the post itself, so the post is both the root and the
parent. NIP-22 needs both scopes: `E`, `K` and `P` for the root, and `e`, `k`
and `p` for the parent.
