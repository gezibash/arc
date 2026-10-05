package direct

import (
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsHex64 says whether text is 64 lower case hex digits: a public key, or a
// SHA-256.
func IsHex64(text string) bool { return hex64.MatchString(text) }

// Offer is one file that a sender gives.
type Offer struct {
	SHA256 string `json:"sha256"`
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	// Modified is the modification time of the file at the offer. The
	// sender refuses a file that changed after it.
	Modified time.Time `json:"modified"`
	// To holds the public keys that can get the file. If it is empty, each
	// caller that reaches the app can get the file.
	To []string `json:"to,omitempty"`
}

// Link is the text that the sender gives to the receiver:
// transfer+arc://<key of the sender>/<sha256>?name=<name>&size=<bytes>.
func Link(sender string, o Offer) string {
	query := url.Values{"name": {o.Name}, "size": {strconv.FormatInt(o.Size, 10)}}
	return "transfer+arc://" + sender + "/" + o.SHA256 + "?" + query.Encode()
}

// ParseLink reads a link. The name becomes a file name with no directory,
// because the name comes from another citizen.
func ParseLink(text string) (sender string, o Offer, err error) {
	u, err := url.Parse(strings.TrimSpace(text))
	if err != nil || u.Scheme != "transfer+arc" {
		return "", o, errors.New("the link must start with transfer+arc://")
	}
	sender = u.Host
	o.SHA256 = strings.TrimPrefix(u.Path, "/")
	if !IsHex64(sender) || !IsHex64(o.SHA256) {
		return "", o, errors.New("the link must hold a public key and a SHA-256, each of 64 hex digits")
	}
	o.Size, err = strconv.ParseInt(u.Query().Get("size"), 10, 64)
	if err != nil || o.Size < 0 {
		return "", o, errors.New("the link has no size")
	}
	o.Name = filepath.Base(filepath.FromSlash(strings.ReplaceAll(u.Query().Get("name"), `\`, "/")))
	if o.Name == "." || o.Name == ".." || o.Name == string(filepath.Separator) || o.Name == "" {
		o.Name = o.SHA256[:16]
	}
	return sender, o, nil
}
