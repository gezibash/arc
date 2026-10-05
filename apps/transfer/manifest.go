// Package transfer holds the manifest of the transfer app, for a program
// that serves the app with no app directory.
package transfer

import _ "embed"

// Manifest is the content of manifest.json.
//
//go:embed manifest.json
var Manifest []byte
