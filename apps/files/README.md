# Files app

Files are private files, sealed with a key of the files app under the data
roots of your devices. The app is a data app: its
[manifest](manifest.json) adds the commands `put`, `list`, `get` and `delete`,
and no service program answers them.

A file is a NIP-94 file metadata event, kind 1063, inside a draft in the
shape of NIP-37, which the device key signs. The
metadata holds the name, the media type, the size, and the SHA-256 hash, in
the tags that NIP-94 defines.

The bytes of the file are the body of the draft, as section 3.9.5 of the
[app interface](../../docs/interface/SPEC.md) defines. The device encrypts
the raw bytes once, and keeps them beside the draft. The wrap names the
encrypted bytes by their SHA-256. A file holds at most 63 MiB.
`arc sync` gives the body to the storage citizens in the blob of the
wrap, and `arc sync --dir` carries it in the directory.

A `file` argument offers `name`, `type`, `size`, and `sha256` to templates.
`get` writes the body, checks the SHA-256 hash of what it writes against
the `x` tag, and refuses a mismatch.

A file that arc v0.30.0 or older put holds its bytes as base64 in the
content, and in parts past 32 KiB. `get` still reads it.
