// Package bleve supplies application search with Bleve's Scorch engine. The index
// is a disposable local projection; it never writes source events.
package bleve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bleve "github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/gezibash/arc/internal/boltlease"
	"github.com/gezibash/arc/internal/search"
	"go.etcd.io/bbolt"
)

type diskIndex struct {
	index bleve.Index
	lock  *bbolt.DB
}

type Index struct {
	mu     sync.Mutex
	lease  *boltlease.Lease[*diskIndex]
	memory bleve.Index
	closed bool
}

// New opens lazily. An empty path keeps the index in memory for this instance.
// A disk index contains readable terms, protected by a private directory.
func New(path string) *Index {
	i := &Index{}
	if path != "" {
		i.lease = boltlease.NewLease(path, func() (*diskIndex, error) {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return nil, err
			}
			// The sidecar lock covers initial metadata creation as well as open indexes.
			// Scorch's root.bolt lock starts too late to serialize a first CLI search.
			lock, err := bbolt.Open(path+".lock.db", 0600, &bbolt.Options{Timeout: 100 * time.Millisecond})
			if err != nil {
				return nil, err
			}
			idx, err := bleve.OpenUsing(path, map[string]interface{}{"bolt_timeout": "100ms"})
			if errors.Is(err, bleve.ErrorIndexPathDoesNotExist) {
				idx, err = createIndex(path)
			}
			if err != nil {
				lock.Close()
				return nil, err
			}
			// Restrict the root before indexing decrypted text, independent of umask.
			if err := os.Chmod(path, 0700); err != nil {
				idx.Close()
				lock.Close()
				return nil, err
			}
			return &diskIndex{index: idx, lock: lock}, nil
		}, func(raw *diskIndex) { raw.index.Close(); raw.lock.Close() })

	}
	return i
}

func createIndex(path string) (bleve.Index, error) {
	m := bleve.NewIndexMapping()
	err := m.AddCustomAnalyzer("arc_words", map[string]interface{}{
		"type": custom.Name, "tokenizer": unicode.Name, "token_filters": []string{lowercase.Name},
	})
	if err != nil {
		return nil, err
	}
	m.DefaultAnalyzer = "arc_words" // Unicode words, lower case, no stop words or stemming.
	m.ScoringModel = "bm25"
	m.IndexDynamic, m.StoreDynamic, m.DocValuesDynamic = false, false, false
	d := bleve.NewDocumentMapping()
	d.Dynamic = false
	for _, name := range []string{"title", "text"} {
		f := bleve.NewTextFieldMapping()
		f.Store, f.DocValues = false, false
		d.AddFieldMappingsAt(name, f)
	}
	m.DefaultMapping = d
	return bleve.NewUsing(path, m, "scorch", bleve.Config.DefaultMemKVStore, map[string]interface{}{"bolt_timeout": "100ms"})
}

func (i *Index) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	if i.lease != nil {
		i.lease.Close()
	}
	if i.memory != nil {
		i.memory.Close()
		i.memory = nil
	}
}

func (i *Index) Search(ctx context.Context, req search.Request) ([]search.Hit, error) {
	var q query.Query = bleve.NewMatchQuery(req.Query)
	if req.Syntax {
		var err error
		q, err = bleve.NewQueryStringQuery(req.Query).Parse()
		if err != nil {
			return nil, fmt.Errorf("search syntax: %w", err)
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil, errors.New("search index is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var hits []search.Hit
	run := func(idx bleve.Index) error {
		var err error
		hits, err = refreshSearch(ctx, idx, req, q)
		return err
	}
	if i.lease != nil {
		err := i.lease.Do(func(raw *diskIndex) error { return run(raw.index) })
		return hits, err
	}
	if i.memory == nil {
		var err error
		// Scorch also supports pathless indexes. Avoid the deprecated upsidedown
		// engine used by Bleve's NewMemOnly convenience constructor.
		var idx bleve.Index
		idx, err = createIndex("")
		if err != nil {
			return nil, err
		}
		i.memory = idx
	}
	err := run(i.memory)
	return hits, err
}

func refreshSearch(ctx context.Context, idx bleve.Index, req search.Request, q query.Query) ([]search.Hit, error) {
	sum := sha256.Sum256([]byte(req.Namespace))
	prefix := hex.EncodeToString(sum[:]) + "/"
	key := []byte(prefix + "versions")
	raw, err := idx.GetInternal(key)
	if err != nil {
		return nil, err
	}
	versions := map[string]string{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &versions); err != nil {
			return nil, fmt.Errorf("search versions: %w", err)
		}
	}
	live := make(map[string]bool, len(req.Sources))
	selected := make(map[string]string)
	batch := idx.NewBatch()
	for _, source := range req.Sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		live[source.ID] = true
		if !source.Selected {
			continue
		}
		id := prefix + source.ID
		selected[id] = source.ID
		if version, ok := versions[source.ID]; ok && version == source.Version {
			continue
		}
		doc, err := req.Load(ctx, source)
		if err != nil {
			return nil, err
		}
		if err := batch.Index(id, map[string]string{"title": doc.Title, "text": doc.Text}); err != nil {
			return nil, err
		}
		versions[source.ID] = source.Version
	}
	for id := range versions {
		if !live[id] {
			batch.Delete(prefix + id)
			delete(versions, id)
		}
	}
	if batch.Size() > 0 {
		raw, err = json.Marshal(versions)
		if err != nil {
			return nil, err
		}
		batch.SetInternal(key, raw)
		if err = idx.Batch(batch); err != nil {
			return nil, err
		}
	}
	if len(selected) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	combined := bleve.NewConjunctionQuery(q, bleve.NewDocIDQuery(ids))
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	request := bleve.NewSearchRequestOptions(combined, limit, 0, false)
	request.SortBy([]string{"-_score", "_id"})
	result, err := idx.SearchInContext(ctx, request)
	if err != nil {
		return nil, err
	}
	hits := make([]search.Hit, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, search.Hit{ID: selected[hit.ID], Score: hit.Score})
	}
	return hits, nil
}
