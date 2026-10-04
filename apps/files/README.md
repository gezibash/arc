# Files app

Files are private files, sealed to your own key. The app is a data app: its
[manifest](manifest.json) adds the commands `put`, `list`, `get` and `delete`,
and no service program answers them.

A file is a NIP-94 file metadata event, kind 1063, inside a NIP-37 draft. The
metadata holds the name, the media type, the size, and the SHA-256 hash, in
the tags that NIP-94 defines. The bytes travel as the content, and in parts
past 32 KiB, as section 3.9.2 of the
[app interface](../../docs/interface/SPEC.md) defines.


A `file` argument offers `name`, `type`, `size`, and `sha256` to templates.
Its bytes go into the content as base64. `save` decodes them, checks the
SHA-256 hash of what it writes against the `x` tag, and refuses a mismatch.
