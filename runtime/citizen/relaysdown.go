package citizen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gezibash/arc/adapters/transport/relay"
)

// relaysDown keeps the relays of one home that did not connect, in the
// file relays-down.json of the home, so that the next command of the home
// does not wait for them again (#439). Each process reads the file at each
// look, and writes it whole through a new file and a rename, so that two
// processes never leave a half file: at worst one loses the record of the
// other. A file that is missing, does not parse or cannot be written is no
// record: a command then behaves as with no file, and note says once why
// it cannot keep a record.
type relaysDown struct {
	path string
	note func(error)
	// snap, when set, is one look at the file for relay.DownFor: a command
	// shorter than that, such as a sync, skips what the home marked when it
	// began, and a mark that runs out or comes in the middle of it changes
	// nothing (#444). A process that runs longer, such as arc serve, looks
	// again after DownFor. Without snap, each look reads the file.
	snap *downSnapshot
}

// downSnapshot is the marks that counted at the time at.
type downSnapshot struct {
	mu    sync.Mutex
	at    time.Time
	marks map[string]downRecord
}

// counts says whether a mark counts at the time at: it ends after at, and
// at most relay.DownFor later. A mark further ahead did not come from arc,
// whose marks end DownFor after they are made: it is no mark, or one wrong
// clock would skip a relay for ever.
func counts(r downRecord, at time.Time) bool {
	until := time.Unix(r.Until, 0)
	return until.After(at) && until.Sub(at) <= relay.DownFor()+time.Second
}

// relaysDownMu makes the read and the write of one change one step in this
// process, so that the relays that one command marks at once all land in
// the file (#444).
var relaysDownMu sync.Mutex

// downRecord is when a relay comes up again, in Unix seconds of the wall
// clock, and why it is down.
type downRecord struct {
	Until int64  `json:"until"`
	Why   string `json:"why"`
}

func (d relaysDown) read() map[string]downRecord {
	body, err := os.ReadFile(d.path)
	if err != nil {
		return map[string]downRecord{}
	}
	var all map[string]downRecord
	if json.Unmarshal(body, &all) != nil || all == nil {
		return map[string]downRecord{}
	}
	return all
}

func (d relaysDown) write(all map[string]downRecord) {
	if err := d.save(all); err != nil && d.note != nil {
		d.note(err)
	}
}

func (d relaysDown) save(all map[string]downRecord) error {
	now := time.Now().Unix()
	for url, r := range all {
		if r.Until <= now {
			delete(all, url)
		}
	}
	body, err := json.Marshal(all)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(d.path), ".relays-down-*")
	if err != nil {
		return err
	}
	_, err = file.Write(body) //arc:raw-ok a JSON file of arc, not a terminal
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), d.path)
	}
	if err != nil {
		_ = os.Remove(file.Name())
		return fmt.Errorf("arc cannot keep the relays that gave no answer in %s: make it a file that arc can write, or remove it", d.path)
	}
	return nil
}

func (d relaysDown) Down(url string) (time.Time, string, bool) {
	var r downRecord
	var ok bool
	if d.snap != nil {
		d.snap.mu.Lock()
		if now := time.Now(); d.snap.marks == nil || now.Sub(d.snap.at) > relay.DownFor() {
			d.snap.at, d.snap.marks = now, map[string]downRecord{}
			for url, r := range d.read() {
				if counts(r, now) {
					d.snap.marks[url] = r
				}
			}
		}
		r, ok = d.snap.marks[url]
		d.snap.mu.Unlock()
	} else if r, ok = d.read()[url]; ok && !counts(r, time.Now()) {
		ok = false
	}
	return time.Unix(r.Until, 0), r.Why, ok
}

func (d relaysDown) MarkDown(url string, until time.Time, why string) {
	relaysDownMu.Lock()
	defer relaysDownMu.Unlock()
	all := d.read()
	all[url] = downRecord{Until: until.Unix(), Why: why}
	d.write(all)
}

func (d relaysDown) Up(url string) {
	relaysDownMu.Lock()
	defer relaysDownMu.Unlock()
	all := d.read()
	if _, ok := all[url]; !ok {
		return
	}
	delete(all, url)
	d.write(all)
}
