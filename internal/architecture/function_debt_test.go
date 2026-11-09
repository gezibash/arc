package architecture

// functionDebt lists each function that was over maxFunctionLines when the
// rule came in, with its length then. It is debt, as docs/proposals/
// small-functions-and-packages.md says. Never add an entry. Split a
// function, then lower or remove its entry.
var functionDebt = map[string]int{
	"adapters/transport/ble/direct.go:Direct.handle":        119,
	"apps/dm/cmd/arc-dm/watch.go:watch":                     101,
	"apps/exec/server/session.go:server.HandleSession":      165,
	"apps/transfer/client/client.go:getter.attempt":         126,
	"cmd/arc-relay/main.go:relayCommand":                    124,
	"cmd/arc-signer/main.go:root":                           147,
	"cmd/arc/bunker.go:serveBunker":                         109,
	"cmd/arc/keys.go:keysCommand":                           221,
	"cmd/arc/lists.go:listsCommand":                         118,
	"cmd/arc/message.go:watchMessages":                      115,
	"cmd/arc/message_command.go:messageCommand":             187,
	"cmd/arc/relay.go:relayCommand":                         128,
	"cmd/arc/serve.go:serve":                                281,
	"cmd/arc/session.go:runSession":                         117,
	"cmd/arc/session_exec.go:execSession":                   151,
	"cmd/arc/sync.go:syncCommand":                           177,
	"cmd/arc/update.go:update":                              137,
	"core/call/session.go:Server.handleSessionFrame":        127,
	"core/keys/wire.go:Key.answer":                          106,
	"core/mail/requests.go:Mail.answer":                     128,
	"runtime/bundle/bundle.go:Load":                         108,
	"runtime/citizen/storagesync.go:Session.SyncStorage":    253,
	"runtime/iface/args.go:bind":                            148,
	"runtime/iface/manifest_check.go:Manifest.check":        113,
	"runtime/iface/manifest_check.go:Manifest.checkCommand": 184,
	"runtime/iface/notebook.go:run.notebookAction":          180,
	"runtime/iface/page_write.go:run.writePage":             102,
	"runtime/iface/publish_sealed.go:run.publishSealed":     196,
	"runtime/selfsync/audit.go:run.audit":                   141,
	"runtime/selfsync/repair.go:run.repair":                 107,
	"runtime/selfsync/repair_plan.go:run.plan":              182,
	"sdk/httpadapter/client.go:ClientSession.Copy":          119,
}
