package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/frame"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/internal/vectors"
	"github.com/gezibash/arc/release"
)

// channelDocument builds one unsigned channel with the releases given.
func channelDocument(publisher *identity.Identity, releases ...map[string]any) map[string]any {
	list := make([]any, 0, len(releases))
	for _, held := range releases {
		list = append(list, held)
	}

	return map[string]any{
		"schema_version": 2,
		"channel":        "stable",
		"publisher":      publisher.EncodePublicKey(),
		"sequence":       3,
		"expires_at":     time.Now().Add(24 * time.Hour).Unix(),
		"releases":       list,
	}
}

func releaseEntry(version, os, arch string, changes ...func(map[string]any)) map[string]any {
	held := map[string]any{
		"version": version, "build": version + "+abc", "runtime": "go1.27.1",
		"platform": map[string]any{"os": os, "arch": arch},
		"size":     1024, "sha256": strings.Repeat("ab", 32),
		"sources": []any{}, "restart_required": true, "withdrawn": false, "eligible": true,
		"install": map[string]any{"size": 1024, "sha256": strings.Repeat("ab", 32)},
	}

	for _, change := range changes {
		change(held)
	}
	return held
}

// The Elixir implementation signed this channel. Go reads it, so both sign
// the same bytes.
func TestVerifiesAnElixirChannel(t *testing.T) {
	want := vectors.Load(t).ReleaseChannel

	publisher, err := identity.FromSeedHex(want.PublisherSeed)
	if err != nil {
		t.Fatal(err)
	}

	channel, err := release.Verify(want.Document, release.Expect{
		Publisher: publisher.PublicKey, Channel: "stable",
		Now: time.Unix(want.Now, 0),
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if channel.Sequence != 7 || len(channel.Releases) != 1 {
		t.Errorf("channel = %+v", channel)
	}
	if channel.Releases[0].Version != "0.7.0" {
		t.Errorf("the release is %+v", channel.Releases[0])
	}

	// A machine of that platform takes it.
	newest, err := channel.Select(release.Platform{OS: "darwin", Arch: "arm64"}, "0.6.0")
	if err != nil || newest.Version != "0.7.0" {
		t.Errorf("select gave %v, %v", newest, err)
	}
}

func TestSignAndVerify(t *testing.T) {
	publisher, _ := identity.Generate()

	signed, err := release.Sign(publisher, channelDocument(publisher, releaseEntry("1.2.3", "linux", "amd64")))
	if err != nil {
		t.Fatal(err)
	}

	channel, err := release.Verify(signed, release.Expect{Publisher: publisher.PublicKey, Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if channel.Name != "stable" || channel.Schema != 2 {
		t.Errorf("channel = %+v", channel)
	}
}

func TestVerifyRefusesWhatItMust(t *testing.T) {
	publisher, _ := identity.Generate()
	other, _ := identity.Generate()

	signed, err := release.Sign(publisher, channelDocument(publisher, releaseEntry("1.0.0", "linux", "amd64")))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := release.Verify(signed, release.Expect{Publisher: other.PublicKey}); err != release.ErrPublisher {
		t.Errorf("another publisher gave %v", err)
	}
	if _, err := release.Verify(signed, release.Expect{Publisher: publisher.PublicKey, Channel: "beta"}); err != release.ErrChannel {
		t.Errorf("another channel gave %v", err)
	}

	// A changed field breaks the signature.
	changed := copyDocument(signed)
	changed["sequence"] = 99
	if _, err := release.Verify(changed, release.Expect{Publisher: publisher.PublicKey}); err != release.ErrSignature {
		t.Errorf("a changed document gave %v", err)
	}

	// A document that has expired is refused.
	old := channelDocument(publisher, releaseEntry("1.0.0", "linux", "amd64"))
	old["expires_at"] = time.Now().Add(-time.Hour).Unix()
	expired, _ := release.Sign(publisher, old)

	if _, err := release.Verify(expired, release.Expect{Publisher: publisher.PublicKey}); err != release.ErrExpired {
		t.Errorf("an expired document gave %v", err)
	}

	// A channel never goes backwards.
	if _, err := release.Verify(signed, release.Expect{
		Publisher: publisher.PublicKey, LastSequence: 10,
	}); err != release.ErrOutOfSequence {
		t.Errorf("an older sequence gave %v", err)
	}

	// One sequence never names two documents.
	if _, err := release.Verify(signed, release.Expect{
		Publisher: publisher.PublicKey, LastSequence: 3, LastDigest: strings.Repeat("cd", 32),
	}); err != release.ErrOutOfSequence {
		t.Errorf("another document of one sequence gave %v", err)
	}

	for name, change := range map[string]func(map[string]any){
		"another schema":        func(d map[string]any) { d["schema_version"] = 9 },
		"another channel":       func(d map[string]any) { d["channel"] = "nightly" },
		"no releases":           func(d map[string]any) { d["releases"] = []any{} },
		"an extra field":        func(d map[string]any) { d["colour"] = "red" },
		"a sequence of nothing": func(d map[string]any) { d["sequence"] = 0 },
	} {
		document := channelDocument(publisher, releaseEntry("1.0.0", "linux", "amd64"))
		change(document)

		if _, err := release.Sign(publisher, document); err == nil {
			t.Errorf("%s: the document signed", name)
		}
	}
}

func TestSelectsTheNewestReleaseOfThisMachine(t *testing.T) {
	publisher, _ := identity.Generate()

	signed, err := release.Sign(publisher, channelDocument(publisher,
		releaseEntry("1.0.0", "linux", "amd64"),
		releaseEntry("1.2.0", "linux", "amd64"),
		releaseEntry("2.0.0", "darwin", "arm64"),
		releaseEntry("1.3.0", "linux", "amd64", func(held map[string]any) { held["withdrawn"] = true }),
	))
	if err != nil {
		t.Fatal(err)
	}

	channel, err := release.Verify(signed, release.Expect{Publisher: publisher.PublicKey})
	if err != nil {
		t.Fatal(err)
	}

	newest, err := channel.Select(release.Platform{OS: "linux", Arch: "amd64"}, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if newest.Version != "1.2.0" {
		t.Errorf("the newest is %s, and a withdrawn release must not count", newest.Version)
	}

	// A machine that runs the newest already takes nothing.
	if _, err := channel.Select(release.Platform{OS: "linux", Arch: "amd64"}, "1.2.0"); err != release.ErrNoRelease {
		t.Errorf("a machine at the newest gave %v", err)
	}
	if _, err := channel.Select(release.Platform{OS: "linux", Arch: "amd64"}, "2.0.0"); err != release.ErrNoRelease {
		t.Errorf("a machine above the newest gave %v", err)
	}

	// A machine of another platform finds nothing.
	if _, err := channel.Select(release.Platform{OS: "windows", Arch: "amd64"}, "1.0.0"); err != release.ErrNoRelease {
		t.Errorf("another platform gave %v", err)
	}
}

// fakeProvider answers as a releases provider does.
type fakeProvider struct {
	document map[string]any
	archive  []byte
}

func (f *fakeProvider) Request(_ context.Context, _ []byte, _ map[string]any, body []byte) (*frame.Frame, error) {
	var request struct {
		Op      string `json:"op"`
		Channel string `json:"channel"`
		Digest  string `json:"digest"`
		Offset  int64  `json:"offset"`
		Length  int    `json:"length"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}

	switch request.Op {
	case "channel":
		encoded, err := json.Marshal(f.document)
		if err != nil {
			return nil, err
		}
		return &frame.Frame{Type: frame.Response, Body: encoded}, nil

	default:
		end := request.Offset + int64(request.Length)
		if end > int64(len(f.archive)) {
			end = int64(len(f.archive))
		}

		encoded, err := json.Marshal(map[string]any{
			"digest": request.Digest, "offset": request.Offset,
			"data": base64Std(f.archive[request.Offset:end]),
		})
		if err != nil {
			return nil, err
		}
		return &frame.Frame{Type: frame.Response, Body: encoded}, nil
	}
}

func TestReadsAChannelAndAnArchive(t *testing.T) {
	publisher, _ := identity.Generate()
	archive := tarball(t, "arc", []byte("the new program"))

	digest := sha256.Sum256(archive)
	entry := releaseEntry("3.0.0", runtime.GOOS, runtime.GOARCH, func(held map[string]any) {
		held["size"] = len(archive)
		held["sha256"] = hex.EncodeToString(digest[:])
		held["install"] = map[string]any{"size": len(archive), "sha256": hex.EncodeToString(digest[:])}
	})

	signed, err := release.Sign(publisher, channelDocument(publisher, entry))
	if err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{document: signed, archive: archive}
	ctx := context.Background()

	document, err := release.FetchChannel(ctx, provider, publisher.PublicKey, "stable")
	if err != nil {
		t.Fatal(err)
	}

	channel, err := release.Verify(document, release.Expect{Publisher: publisher.PublicKey, Channel: "stable"})
	if err != nil {
		t.Fatalf("the channel that came over ARC does not verify: %v", err)
	}

	newest, err := channel.Select(release.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	got, err := release.Download(ctx, provider, publisher.PublicKey, newest.Archive(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, archive) {
		t.Error("the archive did not arrive whole")
	}

	program, err := release.Unpack(got, "arc")
	if err != nil {
		t.Fatal(err)
	}
	if string(program) != "the new program" {
		t.Errorf("the program is %q", program)
	}

	// An archive whose hash differs is refused.
	broken := newest.Archive()
	broken.SHA256 = strings.Repeat("cd", 32)
	if _, err := release.Download(ctx, provider, publisher.PublicKey, broken, nil); err == nil {
		t.Error("an archive that does not match its hash passed")
	}
}

func TestUnpackRefusesAnArchiveThatDoesNotHold(t *testing.T) {
	if _, err := release.Unpack([]byte("not gzip"), "arc"); err == nil {
		t.Error("a file that is not an archive passed")
	}
	if _, err := release.Unpack(tarball(t, "other", []byte("x")), "arc"); err != release.ErrNotInArchive {
		t.Error("an archive without the program passed")
	}
	if _, err := release.Unpack(tarball(t, "../escape", []byte("x")), "escape"); err == nil {
		t.Error("an archive that climbs out passed")
	}

	// The program may stand under a directory.
	program, err := release.Unpack(tarball(t, "arc/bin/arc", []byte("under a directory")), "arc")
	if err != nil {
		t.Fatal(err)
	}
	if string(program) != "under a directory" {
		t.Errorf("the program is %q", program)
	}
}

// A new program runs once before it takes the place of the old one.
func TestReplaceSwapsOneProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this test runs a shell script")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "arc")

	if err := os.WriteFile(path, []byte("#!/bin/sh\necho arc 1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	newer := []byte("#!/bin/sh\necho arc 2.0.0\n")
	if err := release.Replace(path, newer, "2.0.0"); err != nil {
		t.Fatal(err)
	}

	held, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(held, newer) {
		t.Errorf("the program is %q, %v", held, err)
	}

	previous, err := os.ReadFile(path + ".previous")
	if err != nil || !strings.Contains(string(previous), "1.0.0") {
		t.Errorf("the program it replaced is %q, %v", previous, err)
	}

	// A program that reports another version never takes the place.
	if err := release.Replace(path, []byte("#!/bin/sh\necho arc 9.9.9\n"), "3.0.0"); err == nil {
		t.Error("a program of another version took the place")
	}

	held, _ = os.ReadFile(path)
	if !bytes.Equal(held, newer) {
		t.Error("the program changed although the check failed")
	}

	// A program that does not run never takes the place either.
	if err := release.Replace(path, []byte("not a program"), ""); err == nil {
		t.Error("a file that does not run took the place")
	}
}

// tarball writes one file into a gzipped tar archive.
func tarball(t *testing.T, name string, body []byte) []byte {
	t.Helper()

	var out bytes.Buffer
	zipped := gzip.NewWriter(&out)
	writer := tar.NewWriter(zipped)

	if err := writer.WriteHeader(&tar.Header{
		Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	zipped.Close()

	return out.Bytes()
}

func copyDocument(document map[string]any) map[string]any {
	out := make(map[string]any, len(document))
	for key, value := range document {
		out[key] = value
	}
	return out
}
