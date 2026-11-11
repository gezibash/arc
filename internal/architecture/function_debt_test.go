package architecture

// functionDebt lists each function that was over maxFunctionLines when the
// rule came in, with its length then. It is debt, as docs/proposals/
// small-functions-and-packages.md says. Never add a function. Split a
// function, then lower or remove its entry. A move of a listed function,
// unchanged, to another file changes only the key of its entry.
var functionDebt = map[string]int{
	"adapters/transport/ble/direct.go:Direct.handle":     119,
	"apps/dm/cmd/arc-dm/watch.go:watch":                  101,
	"apps/exec/server/session.go:server.HandleSession":   165,
	"apps/transfer/client/client.go:getter.attempt":      126,
	"core/call/session.go:Server.handleSessionFrame":     127,
	"core/keys/wire.go:Key.answer":                       106,
	"core/mail/requests.go:Mail.answer":                  128,
	"runtime/citizen/storagesync.go:Session.SyncStorage": 253,
	"runtime/selfsync/audit.go:run.audit":                141,
	"runtime/selfsync/repair.go:run.repair":              107,
	"runtime/selfsync/repair_plan.go:run.plan":           182,
	"sdk/httpadapter/client.go:ClientSession.Copy":       119,
}
