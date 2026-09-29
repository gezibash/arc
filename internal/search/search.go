// Package search shares request and result types between application search
// consumers and adapters. These types define no ARC protocol behavior.
package search

import "context"

// Source names a current document and its content version. Selected documents
// are eligible for this request. Unselected documents need no body fetch.
type Source struct {
	ID, Version string
	Selected    bool
}

type Document struct{ Title, Text string }

type Request struct {
	// Namespace separates identities and capabilities sharing an index.
	Namespace string
	// Sources is the complete current inventory for this namespace, including
	// unselected documents. Omitted documents must be removed from the index.
	Sources []Source
	// Load is called only for selected documents whose version is not indexed.
	Load  func(context.Context, Source) (Document, error)
	Query string
	// Syntax enables the engine's query language. Plain queries match any word.
	Syntax bool
	Limit  int
}

type Hit struct {
	ID    string
	Score float64
}
