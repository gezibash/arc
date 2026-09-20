package toolbox_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/toolbox"
)

func newCache(t *testing.T) (*toolbox.Cache, *identity.Identity) {
	t.Helper()

	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return &toolbox.Cache{Dir: t.TempDir()}, me
}

func TestOneCommandDoesNotTurnOnAnother(t *testing.T) {
	cache, me := newCache(t)

	if err := cache.Enable("dm"); err != nil {
		t.Fatal(err)
	}
	if cache.On("journal") {
		t.Error("turning dm on turned journal on as well")
	}

	kept, err := cache.Keep("journal", me, thread("j1\tin\tivy\t2026-01-01T10:00:00Z\t-\tread\thello"))
	if err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Errorf("journal kept %d records while off", kept)
	}
}

func TestTheCacheKeepsNothingWhileItIsOff(t *testing.T) {
	cache, me := newCache(t)

	if cache.On("dm") {
		t.Error("a new cache is on")
	}

	kept, err := cache.Keep("dm", me, thread("m1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\thello"))
	if err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Errorf("kept %d records while off", kept)
	}
}

func TestTheCacheKeepsAndReadsRecords(t *testing.T) {
	cache, me := newCache(t)

	if err := cache.Enable("dm"); err != nil {
		t.Fatal(err)
	}
	if !cache.On("dm") {
		t.Fatal("the cache is off after enable")
	}

	text := thread(
		"m1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\tthe meeting is at noon",
		"m2\tout\trose\t2026-01-01T10:05:00Z\tm1\tdelivered\tI will be there")

	kept, err := cache.Keep("dm", me, text)
	if err != nil {
		t.Fatal(err)
	}
	if kept != 2 {
		t.Fatalf("kept %d records, want 2", kept)
	}

	// The same answer twice keeps nothing new. A record is named by its id.
	again, err := cache.Keep("dm", me, text)
	if err != nil || again != 0 {
		t.Errorf("the second answer kept %d records, %v", again, err)
	}

	found, err := cache.Search("dm", me, "meeting")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != "m1" {
		t.Fatalf("search found %+v", found)
	}

	all, err := cache.Search("", me, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].T > all[1].T {
		t.Errorf("an empty search found %d records, out of order", len(all))
	}
}

func TestTheCacheSealsItsRecords(t *testing.T) {
	cache, me := newCache(t)
	if err := cache.Enable("dm"); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.Keep("dm", me, thread("m1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\tthe secret")); err != nil {
		t.Fatal(err)
	}

	var body []byte
	err := filepath.Walk(cache.Dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".sealed") {
			body, err = os.ReadFile(path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("no record on disk")
	}
	if strings.Contains(string(body), "the secret") {
		t.Error("the record is on disk in the clear")
	}

	// Another identity reads nothing, because the record is sealed.
	other, _ := identity.Generate()
	found, err := cache.Search("dm", other, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("another identity read %d records", len(found))
	}
}

func TestTheCacheCountsAndClears(t *testing.T) {
	cache, me := newCache(t)
	for _, name := range []string{"dm", "journal"} {
		if err := cache.Enable(name); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := cache.Keep("dm", me, thread("m1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\tone")); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Keep("journal", me, thread("j1\tin\tivy\t2026-01-02T10:00:00Z\t-\tread\ttwo")); err != nil {
		t.Fatal(err)
	}

	total, err := cache.Count("", me.PublicKey)
	if err != nil || total != 2 {
		t.Fatalf("count = %d, %v", total, err)
	}

	commands := cache.Commands(me.PublicKey)
	if len(commands) != 2 {
		t.Errorf("commands = %v", commands)
	}

	removed, err := cache.Clear("dm", me.PublicKey)
	if err != nil || removed != 1 {
		t.Fatalf("clear dm removed %d, %v", removed, err)
	}

	total, err = cache.Count("", me.PublicKey)
	if err != nil || total != 1 {
		t.Fatalf("count after clear = %d, %v", total, err)
	}

	removed, err = cache.Clear("", me.PublicKey)
	if err != nil || removed != 1 {
		t.Fatalf("clear all removed %d, %v", removed, err)
	}
	if total, _ := cache.Count("", me.PublicKey); total != 0 {
		t.Errorf("the cache still holds %d records", total)
	}
}

func TestDisableKeepsTheRecords(t *testing.T) {
	cache, me := newCache(t)
	if err := cache.Enable("dm"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Keep("dm", me, thread("m1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\tone")); err != nil {
		t.Fatal(err)
	}

	if err := cache.Disable("dm"); err != nil {
		t.Fatal(err)
	}
	if cache.On("dm") {
		t.Error("the cache is on after disable")
	}

	found, err := cache.Search("dm", me, "one")
	if err != nil || len(found) != 1 {
		t.Errorf("the records went away: %d, %v", len(found), err)
	}

	// Disable twice is not an error.
	if err := cache.Disable("dm"); err != nil {
		t.Errorf("the second disable failed: %v", err)
	}
}
