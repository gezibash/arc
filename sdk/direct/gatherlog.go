package direct

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pion/logging"
)

// gatherLog keeps the warnings of the ICE agent of a relay attempt. A relay
// attempt gathers only through the TURN server, so they say why a dial, a
// TLS handshake or an allocation failed. pion logs why it gathered no address on the TURN server only
// there, so an error that says the TURN server gave no address can give
// the reason (#427). It keeps a few short lines, and prints nothing.
type gatherLog struct {
	mu    sync.Mutex
	lines []string
}

// maxGatherLines is the number of lines that a gather log keeps.
const maxGatherLines = 3

func (g *gatherLog) NewLogger(scope string) logging.LeveledLogger {
	return gatherLogger{log: g, scope: scope}
}

func (g *gatherLog) add(text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.lines) < maxGatherLines {
		g.lines = append(g.lines, strings.TrimSpace(text[:min(len(text), 200)]))
	}
}

// reason is the text that follows "the TURN server of the relay gave no
// address", or "" when pion logged no reason.
func (g *gatherLog) reason() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.lines) == 0 {
		return ""
	}
	return ": " + strings.Join(g.lines, "; ")
}

// gatherLogger passes on the warnings and errors of the ICE agent.
type gatherLogger struct {
	log   *gatherLog
	scope string
}

func (l gatherLogger) keep(text string) {
	if l.scope == "ice" {
		l.log.add(text)
	}
}

func (gatherLogger) Trace(string)                  {}
func (gatherLogger) Tracef(string, ...any)         {}
func (gatherLogger) Debug(string)                  {}
func (gatherLogger) Debugf(string, ...any)         {}
func (gatherLogger) Info(string)                   {}
func (gatherLogger) Infof(string, ...any)          {}
func (l gatherLogger) Warn(msg string)             { l.keep(msg) }
func (l gatherLogger) Warnf(f string, args ...any) { l.keep(fmt.Sprintf(f, args...)) }
func (l gatherLogger) Error(msg string)            { l.keep(msg) }
func (l gatherLogger) Errorf(f string, args ...any) {
	l.keep(fmt.Sprintf(f, args...))
}
