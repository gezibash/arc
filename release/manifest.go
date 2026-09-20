// Package release reads the signed channel document of ARC, and says which
// release a machine should run.
//
// A channel document names every release of one channel. Its publisher signs
// the canonical JSON of the document, under a domain that names the schema:
//
//	ARC-RELEASE-CHANNEL-V<schema>\0<canonical JSON without the signature>
//
// Nothing here downloads or installs. It establishes who published a
// channel, and which release of it fits this machine.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/internal/canonical"
)

// Domain stands at the front of the bytes that a publisher signs.
const Domain = "ARC-RELEASE-CHANNEL-V"

// The channels that a publisher may name.
var Channels = map[string]bool{"stable": true, "beta": true}

// The schema versions that this build reads.
var schemas = map[int64]bool{1: true, 2: true}

// Errors of this package.
var (
	ErrInvalid       = errors.New("release: the channel document does not hold together")
	ErrPublisher     = errors.New("release: another publisher signed this channel")
	ErrChannel       = errors.New("release: the document names another channel")
	ErrSignature     = errors.New("release: the signature does not verify")
	ErrExpired       = errors.New("release: the channel document has expired")
	ErrOutOfSequence = errors.New("release: the channel went backwards")
	ErrNoRelease     = errors.New("release: no release fits this machine")
)

var hexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Platform names one build of one machine.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// Release is one build that a channel names.
type Release struct {
	Version         string
	Build           string
	Runtime         string
	Platform        Platform
	Size            int64
	SHA256          string
	RestartRequired bool
	Withdrawn       bool
	Eligible        bool
	// Install names the whole archive, when the release carries one.
	Install *Artifact
}

// Artifact is one file that a citizen downloads.
type Artifact struct {
	Size   int64
	SHA256 string
}

// Channel is one verified channel document.
type Channel struct {
	Schema    int64
	Name      string
	Publisher []byte
	Sequence  int64
	ExpiresAt int64
	Releases  []Release
	// Digest names this document, so a citizen never takes an older one.
	Digest string
}

// Expect holds what a citizen expects of a channel.
type Expect struct {
	// Publisher is the key that the citizen pinned. It is needed.
	Publisher []byte
	// Channel is the name that the citizen asked for.
	Channel string
	// LastSequence is the sequence that the citizen saw before.
	LastSequence int64
	// LastDigest names the document of that sequence.
	LastDigest string
	// Now is the time to check the expiry against. The zero time means now.
	Now time.Time
}

// Sign signs one channel document. A publisher signs the canonical JSON of
// the document, under the domain of its schema.
func Sign(me *identity.Identity, unsigned map[string]any) (map[string]any, error) {
	channel, err := readChannel(unsigned)
	if err != nil {
		return nil, err
	}
	if string(channel.Publisher) != string(me.PublicKey) {
		return nil, ErrPublisher
	}

	encoded, err := canonical.Encode(unsigned)
	if err != nil {
		return nil, ErrInvalid
	}

	payload := append([]byte(Domain+strconv.FormatInt(channel.Schema, 10)+"\x00"), encoded...)

	signed := make(map[string]any, len(unsigned)+1)
	for key, value := range unsigned {
		signed[key] = value
	}
	signed["signature"] = map[string]any{
		"algorithm": "ed25519",
		"value":     hex.EncodeToString(me.Sign(payload)),
	}
	return signed, nil
}

// Verify reads one signed channel document.
func Verify(document map[string]any, expect Expect) (*Channel, error) {
	signature, _ := document["signature"].(map[string]any)
	if signature == nil || signature["algorithm"] != "ed25519" {
		return nil, ErrInvalid
	}

	unsigned := make(map[string]any, len(document)-1)
	for key, value := range document {
		if key != "signature" {
			unsigned[key] = value
		}
	}

	channel, err := readChannel(unsigned)
	if err != nil {
		return nil, err
	}

	if len(expect.Publisher) != identity.SeedBytes {
		return nil, ErrPublisher
	}
	if string(channel.Publisher) != string(expect.Publisher) {
		return nil, ErrPublisher
	}
	if expect.Channel != "" && channel.Name != expect.Channel {
		return nil, ErrChannel
	}

	encoded, err := canonical.Encode(unsigned)
	if err != nil {
		return nil, ErrInvalid
	}

	payload := append([]byte(Domain+strconv.FormatInt(channel.Schema, 10)+"\x00"), encoded...)

	value, err := hex.DecodeString(text(signature["value"]))
	if err != nil || len(value) != 64 {
		return nil, ErrSignature
	}
	if !identity.Verify(channel.Publisher, payload, value) {
		return nil, ErrSignature
	}

	digest := sha256.Sum256(encoded)
	channel.Digest = hex.EncodeToString(digest[:])

	now := expect.Now
	if now.IsZero() {
		now = time.Now()
	}
	if channel.ExpiresAt <= now.Unix() {
		return nil, ErrExpired
	}

	// A channel never goes backwards, and one sequence never names two
	// documents.
	switch {
	case channel.Sequence < expect.LastSequence:
		return nil, ErrOutOfSequence
	case channel.Sequence == expect.LastSequence && expect.LastDigest != "" &&
		channel.Digest != expect.LastDigest:
		return nil, ErrOutOfSequence
	}

	return channel, nil
}

// Select returns the newest release of this channel that fits a machine, and
// says whether it stands above the running version.
func (c *Channel) Select(platform Platform, version string) (*Release, error) {
	running, runningOK := parseVersion(version)

	var fitting []Release
	for _, held := range c.Releases {
		if held.Withdrawn || held.Platform != platform {
			continue
		}
		fitting = append(fitting, held)
	}

	if len(fitting) == 0 {
		return nil, ErrNoRelease
	}

	sort.Slice(fitting, func(left, right int) bool {
		return laterThan(fitting[left].Version, fitting[right].Version)
	})

	newest := fitting[0]
	if !runningOK {
		return &newest, nil
	}

	held, ok := parseVersion(newest.Version)
	if !ok || !later(held, running) {
		return nil, ErrNoRelease
	}
	return &newest, nil
}

// Archive names the file to download for one release.
func (r *Release) Archive() *Artifact {
	if r.Install != nil {
		return r.Install
	}
	return &Artifact{Size: r.Size, SHA256: r.SHA256}
}

func readChannel(unsigned map[string]any) (*Channel, error) {
	expected := []string{"schema_version", "channel", "publisher", "sequence", "expires_at", "releases"}
	if len(unsigned) != len(expected) {
		return nil, ErrInvalid
	}
	for _, name := range expected {
		if _, held := unsigned[name]; !held {
			return nil, ErrInvalid
		}
	}

	schema, ok := wholeNumber(unsigned["schema_version"])
	if !ok || !schemas[schema] {
		return nil, ErrInvalid
	}

	name := text(unsigned["channel"])
	if !Channels[name] {
		return nil, ErrInvalid
	}

	publisher, err := hex.DecodeString(text(unsigned["publisher"]))
	if err != nil || len(publisher) != identity.SeedBytes {
		return nil, ErrInvalid
	}

	sequence, ok := wholeNumber(unsigned["sequence"])
	if !ok || sequence <= 0 {
		return nil, ErrInvalid
	}

	expires, ok := wholeNumber(unsigned["expires_at"])
	if !ok || expires <= 0 {
		return nil, ErrInvalid
	}

	list, _ := unsigned["releases"].([]any)
	if len(list) == 0 {
		return nil, ErrInvalid
	}

	releases := make([]Release, 0, len(list))
	seen := map[string]bool{}

	for _, item := range list {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, ErrInvalid
		}

		held, err := readRelease(fields, name)
		if err != nil {
			return nil, err
		}

		key := held.Build + "|" + held.Platform.OS + "|" + held.Platform.Arch
		if seen[key] {
			return nil, ErrInvalid
		}
		seen[key] = true

		releases = append(releases, *held)
	}

	return &Channel{
		Schema: schema, Name: name, Publisher: publisher,
		Sequence: sequence, ExpiresAt: expires, Releases: releases,
	}, nil
}

func readRelease(fields map[string]any, channel string) (*Release, error) {
	platform, _ := fields["platform"].(map[string]any)
	if platform == nil || len(platform) != 2 {
		return nil, ErrInvalid
	}

	size, sizeOK := wholeNumber(fields["size"])
	restart, restartOK := fields["restart_required"].(bool)
	withdrawn, withdrawnOK := fields["withdrawn"].(bool)
	eligible, eligibleOK := fields["eligible"].(bool)

	version := text(fields["version"])
	if _, ok := parseVersion(version); !ok {
		return nil, ErrInvalid
	}
	// A stable channel never names a release before its own version.
	if channel == "stable" && strings.Contains(version, "-") {
		return nil, ErrInvalid
	}

	if !sizeOK || size <= 0 || !restartOK || !withdrawnOK || !eligibleOK {
		return nil, ErrInvalid
	}
	if !hexPattern.MatchString(text(fields["sha256"])) {
		return nil, ErrInvalid
	}
	if text(fields["build"]) == "" || text(fields["runtime"]) == "" {
		return nil, ErrInvalid
	}

	held := &Release{
		Version: version, Build: text(fields["build"]), Runtime: text(fields["runtime"]),
		Platform: Platform{OS: text(platform["os"]), Arch: text(platform["arch"])},
		Size:     size, SHA256: text(fields["sha256"]),
		RestartRequired: restart, Withdrawn: withdrawn, Eligible: eligible,
	}

	if held.Platform.OS == "" || held.Platform.Arch == "" {
		return nil, ErrInvalid
	}

	if install, ok := fields["install"].(map[string]any); ok {
		size, ok := wholeNumber(install["size"])
		if !ok || size <= 0 || len(install) != 2 || !hexPattern.MatchString(text(install["sha256"])) {
			return nil, ErrInvalid
		}
		held.Install = &Artifact{Size: size, SHA256: text(install["sha256"])}
	}
	return held, nil
}

// version is one release version, read as numbers and a pre-release part.
type version struct {
	major, minor, patch int
	pre                 string
}

var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func parseVersion(value string) (version, bool) {
	groups := versionPattern.FindStringSubmatch(value)
	if groups == nil {
		return version{}, false
	}

	major, _ := strconv.Atoi(groups[1])
	minor, _ := strconv.Atoi(groups[2])
	patch, _ := strconv.Atoi(groups[3])
	return version{major: major, minor: minor, patch: patch, pre: groups[4]}, true
}

// later says whether one version stands above another. A version with a
// pre-release part stands below the same version without one.
func later(left, right version) bool {
	switch {
	case left.major != right.major:
		return left.major > right.major
	case left.minor != right.minor:
		return left.minor > right.minor
	case left.patch != right.patch:
		return left.patch > right.patch
	case left.pre == right.pre:
		return false
	case left.pre == "":
		return true
	case right.pre == "":
		return false
	default:
		return left.pre > right.pre
	}
}

func laterThan(left, right string) bool {
	first, firstOK := parseVersion(left)
	second, secondOK := parseVersion(right)

	switch {
	case firstOK && secondOK:
		return later(first, second)
	case firstOK:
		return true
	default:
		return false
	}
}

func text(value any) string {
	out, _ := value.(string)
	return out
}

func wholeNumber(value any) (int64, bool) {
	switch value := value.(type) {
	case int64:
		return value, true
	case int:
		return int64(value), true
	case float64:
		return int64(value), value == float64(int64(value))
	default:
		type number interface{ Int64() (int64, error) }
		if held, ok := value.(number); ok {
			out, err := held.Int64()
			return out, err == nil
		}
		return 0, false
	}
}
