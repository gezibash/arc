package release

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

// A citizen remembers the newest channel document that it accepted. Without
// that memory, a publisher, a provider, or anyone between them could serve
// an older signed document and hold the citizen on an old release. The
// signature of an old document still verifies, because it was real once.

var channelName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Checkpoint holds what this machine accepted before.
type Checkpoint struct {
	// Dir is the directory of ARC, for example ~/.config/arc.
	Dir string
}

// record is the file of one channel of one publisher.
type record struct {
	Sequence int64  `json:"sequence"`
	Digest   string `json:"digest"`
}

func (c *Checkpoint) path(publisher []byte, channel string) (string, error) {
	if !channelName.MatchString(channel) {
		return "", errors.New("release: the channel name holds a character that is not allowed")
	}
	return filepath.Join(c.Dir, "update", hex.EncodeToString(publisher), channel+".json"), nil
}

// Read answers what this machine accepted before. A channel that it never
// read answers a zero sequence, which accepts any document.
func (c *Checkpoint) Read(publisher []byte, channel string) (Expect, error) {
	expect := Expect{Publisher: publisher, Channel: channel}

	path, err := c.path(publisher, channel)
	if err != nil {
		return expect, err
	}

	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return expect, nil
	}
	if err != nil {
		return expect, err
	}

	var held record
	if err := json.Unmarshal(body, &held); err != nil {
		// A file that cannot be read is not a reason to accept an old
		// document. Refuse, and let the operator look.
		return expect, errors.New("release: the update checkpoint is damaged: " + path)
	}

	expect.LastSequence = held.Sequence
	expect.LastDigest = held.Digest
	return expect, nil
}

// Write remembers a document that verified. It writes through a temporary
// file, so an interrupted write leaves the old record in place.
func (c *Checkpoint) Write(publisher []byte, channel *Channel) error {
	path, err := c.path(publisher, channel.Name)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	body, err := json.Marshal(record{Sequence: channel.Sequence, Digest: channel.Digest})
	if err != nil {
		return err
	}

	temporary := path + ".new"
	if err := os.WriteFile(temporary, body, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
