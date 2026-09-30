# Direct messages app

Direct messages are private messages, as NIP-17 defines. The app is a data
app: its [manifest](manifest.json) adds the commands `send`, `inbox` and
`open`, and no service program answers them. A NIP-17 client opens the
messages. The [app interface](../../docs/interface/SPEC.md) defines the
primitives that the manifest uses.

```sh
arc install <manifest-author> dm
arc help dm
```
