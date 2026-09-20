// Command sqlite-provider answers SQL over ARC.
//
// The operator names each database and who may read or write it. A request
// names one database by the path of its invocation, for example /main. A
// caller never names a file.
//
// The authorizer of SQLite is the boundary: it refuses to attach a file, to
// read a pragma, to run a transaction of its own, and, for a reader, to
// change anything.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/gezibash/arc/go/provider"
)

var databaseNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var pathPattern = regexp.MustCompile(`^/[a-z][a-z0-9_-]{0,63}$`)

// The limits of a request, and the ceiling of each one.
var (
	defaultLimits = limits{
		BodyBytes: 256 * 1024, SQLBytes: 64 * 1024, BatchStatements: 32,
		Rows: 1000, OutputBytes: 1024 * 1024, QueryMS: 5000, BusyMS: 1000,
	}
	maxLimits = limits{
		BodyBytes: 1024 * 1024, SQLBytes: 1024 * 1024, BatchStatements: 256,
		Rows: 10_000, OutputBytes: 1024 * 1024, QueryMS: 30_000, BusyMS: 10_000,
	}
)

type limits struct {
	BodyBytes       int `json:"body_bytes"`
	SQLBytes        int `json:"sql_bytes"`
	BatchStatements int `json:"batch_statements"`
	Rows            int `json:"rows"`
	OutputBytes     int `json:"output_bytes"`
	QueryMS         int `json:"query_ms"`
	BusyMS          int `json:"busy_ms"`
}

// database is one database of the operator, and who may reach it.
type database struct {
	Path   string            `json:"path"`
	Grants map[string]string `json:"grants"`
}

type configFile struct {
	Databases map[string]database `json:"databases"`
	Limits    *limits             `json:"limits"`
}

type config struct {
	Databases map[string]database
	Limits    limits
}

// loadConfig reads SQLITE_CONFIG and checks every field. A configuration
// that is not complete stops the program.
func loadConfig() (*config, error) {
	path, err := provider.ConfigPath("SQLITE_CONFIG")
	if err != nil {
		return nil, invalidConfig()
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil, invalidConfig()
	}

	var file configFile
	if err := provider.ReadConfig(path, &file); err != nil {
		return nil, invalidConfig()
	}
	if len(file.Databases) == 0 {
		return nil, invalidConfig()
	}

	for name, held := range file.Databases {
		if !databaseNamePattern.MatchString(name) {
			return nil, invalidConfig()
		}
		if !filepath.IsAbs(held.Path) {
			return nil, invalidConfig()
		}
		if info, err := os.Lstat(held.Path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, invalidConfig()
		}
		if len(held.Grants) == 0 {
			return nil, invalidConfig()
		}
		for key, role := range held.Grants {
			if !publicKeyPattern.MatchString(key) || (role != "read" && role != "write") {
				return nil, invalidConfig()
			}
		}
	}

	checked := defaultLimits
	if file.Limits != nil {
		checked = merge(*file.Limits, defaultLimits)
		if err := checkLimits(checked); err != nil {
			return nil, err
		}
	}

	return &config{Databases: file.Databases, Limits: checked}, nil
}

func merge(given, fallback limits) limits {
	if given.BodyBytes == 0 {
		given.BodyBytes = fallback.BodyBytes
	}
	if given.SQLBytes == 0 {
		given.SQLBytes = fallback.SQLBytes
	}
	if given.BatchStatements == 0 {
		given.BatchStatements = fallback.BatchStatements
	}
	if given.Rows == 0 {
		given.Rows = fallback.Rows
	}
	if given.OutputBytes == 0 {
		given.OutputBytes = fallback.OutputBytes
	}
	if given.QueryMS == 0 {
		given.QueryMS = fallback.QueryMS
	}
	if given.BusyMS == 0 {
		given.BusyMS = fallback.BusyMS
	}
	return given
}

func checkLimits(given limits) error {
	for _, limit := range []struct {
		value int
		max   int
	}{
		{given.BodyBytes, maxLimits.BodyBytes},
		{given.SQLBytes, maxLimits.SQLBytes},
		{given.BatchStatements, maxLimits.BatchStatements},
		{given.Rows, maxLimits.Rows},
		{given.OutputBytes, maxLimits.OutputBytes},
		{given.QueryMS, maxLimits.QueryMS},
		{given.BusyMS, maxLimits.BusyMS},
	} {
		if limit.value < 1 || limit.value > limit.max {
			return invalidConfig()
		}
	}
	return nil
}

func invalidConfig() error { return fmt.Errorf("invalid SQLITE_CONFIG") }

var publicKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
