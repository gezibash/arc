package citizen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// relaysDown keeps the relays of one home that did not connect, in the
// file relays-down.json of the home, so that the next command of the home
// does not wait for them again (#439). Each process reads the file at each
// look, and writes it whole through a new file and a rename, so that two
// processes never leave a half file: at worst one loses the record of the
// other. A file that is missing, does not parse or cannot be written is no
// record: a command then behaves as with no file.
type relaysDown struct{ path string }

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
	now := time.Now().Unix()
	for url, r := range all {
		if r.Until <= now {
			delete(all, url)
		}
	}
	body, err := json.Marshal(all)
	if err != nil {
		return
	}
	file, err := os.CreateTemp(filepath.Dir(d.path), ".relays-down-*")
	if err != nil {
		return
	}
	_, err = file.Write(body) //arc:raw-ok a JSON file of arc, not a terminal
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil || os.Rename(file.Name(), d.path) != nil {
		_ = os.Remove(file.Name())
	}
}

func (d relaysDown) Down(url string) (time.Time, string, bool) {
	r, ok := d.read()[url]
	return time.Unix(r.Until, 0), r.Why, ok
}

func (d relaysDown) MarkDown(url string, until time.Time, why string) {
	all := d.read()
	all[url] = downRecord{Until: until.Unix(), Why: why}
	d.write(all)
}

func (d relaysDown) Up(url string) {
	all := d.read()
	if _, ok := all[url]; !ok {
		return
	}
	delete(all, url)
	d.write(all)
}
