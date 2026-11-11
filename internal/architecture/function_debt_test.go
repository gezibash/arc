package architecture

// functionDebt lists each function that was over maxFunctionLines when the
// rule came in, with its length then. It is debt, as docs/proposals/
// small-functions-and-packages.md says. Never add a function. Split a
// function, then lower or remove its entry. A move of a listed function,
// unchanged, to another file changes only the key of its entry.
var functionDebt = map[string]int{}
