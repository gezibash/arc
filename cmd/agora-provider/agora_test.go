package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/provider"
)

func testBoard(t *testing.T) (*server, *identity.Identity) {
	t.Helper()

	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	board := me.EncodePublicKey()
	held := &store{root: t.TempDir(), board: board, maxPosts: DefaultMaxPosts}
	if err := held.ensure(); err != nil {
		t.Fatal(err)
	}
	return &server{store: held, board: board}, me
}

// sign builds one post as the CLI of a citizen does.
func sign(t *testing.T, author *identity.Identity, board, body string, parent *string) *post {
	t.Helper()

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}

	held := &post{
		Version:   1,
		Author:    author.EncodePublicKey(),
		Board:     board,
		Body:      body,
		Parent:    parent,
		CreatedAt: time.Now().Unix(),
		Nonce:     hex.EncodeToString(nonce),
	}

	message, err := signatureMessage(held)
	if err != nil {
		t.Fatal(err)
	}

	signature := author.Sign([]byte(message))
	held.Signature = hex.EncodeToString(signature)
	held.ID = sum(append([]byte(message), signature...))
	return held
}

func send(t *testing.T, s *server, from string, body map[string]any) (map[string]any, error) {
	t.Helper()

	message, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	reply, err := s.HandleRequest(context.Background(), provider.Request{
		From: from, Message: string(message),
		Meta: map[string]any{"method": "RAW", "path": "/"},
	})
	if err != nil {
		return nil, err
	}

	var answer map[string]any
	if err := json.Unmarshal([]byte(reply), &answer); err != nil {
		t.Fatalf("the reply is not JSON: %q", reply)
	}
	return answer, nil
}

func publish(t *testing.T, s *server, author *identity.Identity, body string, parent *string) *post {
	t.Helper()

	held := sign(t, author, s.board, body, parent)
	if _, err := send(t, s, held.Author, map[string]any{"op": "post", "post": held}); err != nil {
		t.Fatalf("post %q: %v", body, err)
	}
	return held
}

func TestPostReadAndFeed(t *testing.T) {
	s, me := testBoard(t)

	first := publish(t, s, me, "hello republic", nil)
	second := publish(t, s, me, "and again", nil)

	got, err := send(t, s, me.EncodePublicKey(), map[string]any{"op": "read", "id": first.ID})
	if err != nil {
		t.Fatal(err)
	}
	held, _ := got["post"].(map[string]any)
	if held["body"] != "hello republic" || held["author"] != me.EncodePublicKey() {
		t.Errorf("read = %v", held)
	}

	feed, err := send(t, s, me.EncodePublicKey(), map[string]any{"op": "feed"})
	if err != nil {
		t.Fatal(err)
	}

	posts, _ := feed["posts"].([]any)
	if len(posts) != 2 {
		t.Fatalf("the feed holds %d posts", len(posts))
	}
	if feed["next"] != nil {
		t.Errorf("next = %v", feed["next"])
	}

	// The feed follows the order in which the board took the posts.
	one, _ := posts[0].(map[string]any)
	two, _ := posts[1].(map[string]any)
	if one["id"] != first.ID || two["id"] != second.ID {
		t.Errorf("the feed is out of order")
	}
}

func TestRepliesFormAThread(t *testing.T) {
	s, me := testBoard(t)
	other, _ := identity.Generate()

	parent := publish(t, s, me, "the question", nil)
	reply := publish(t, s, other, "the answer", &parent.ID)

	// A reply never stands in the feed.
	feed, err := send(t, s, me.EncodePublicKey(), map[string]any{"op": "feed"})
	if err != nil {
		t.Fatal(err)
	}
	if posts, _ := feed["posts"].([]any); len(posts) != 1 {
		t.Errorf("the feed holds %d posts", len(posts))
	}

	thread, err := send(t, s, me.EncodePublicKey(), map[string]any{"op": "thread", "id": parent.ID})
	if err != nil {
		t.Fatal(err)
	}

	posts, _ := thread["posts"].([]any)
	if len(posts) != 1 {
		t.Fatalf("the thread holds %d replies", len(posts))
	}

	one, _ := posts[0].(map[string]any)
	if one["id"] != reply.ID {
		t.Errorf("the thread holds %v", one["id"])
	}

	// The thread names the post that it answers.
	held, _ := thread["post"].(map[string]any)
	if held["id"] != parent.ID {
		t.Errorf("the thread names %v", held["id"])
	}

	// A reply to a post that is not there is refused.
	missing := strings.Repeat("ab", 32)
	orphan := sign(t, me, s.board, "no parent", &missing)
	if _, err := send(t, s, orphan.Author, map[string]any{"op": "post", "post": orphan}); err == nil {
		t.Error("a reply to a post that is not there landed")
	}
}

func TestTheFeedPages(t *testing.T) {
	s, me := testBoard(t)

	for index := 0; index < 5; index++ {
		publish(t, s, me, "post "+strings.Repeat("x", index+1), nil)
	}

	seen := 0
	after := 0

	for pages := 0; pages < 10; pages++ {
		request := map[string]any{"op": "feed", "limit": 2}
		if after > 0 {
			request["after"] = after
		}

		page, err := send(t, s, me.EncodePublicKey(), request)
		if err != nil {
			t.Fatal(err)
		}

		posts, _ := page["posts"].([]any)
		seen += len(posts)

		if page["next"] == nil {
			break
		}
		after = int(page["next"].(float64))
	}

	if seen != 5 {
		t.Errorf("the pages held %d posts, want 5", seen)
	}
}

func TestTheSamePostAgainIsNotAnError(t *testing.T) {
	s, me := testBoard(t)
	held := publish(t, s, me, "once", nil)

	if _, err := send(t, s, held.Author, map[string]any{"op": "post", "post": held}); err != nil {
		t.Errorf("the same post again failed: %v", err)
	}

	feed, _ := send(t, s, me.EncodePublicKey(), map[string]any{"op": "feed"})
	if posts, _ := feed["posts"].([]any); len(posts) != 1 {
		t.Errorf("the board holds %d posts", len(posts))
	}
}

func TestAPostThatDoesNotHold(t *testing.T) {
	s, me := testBoard(t)
	other, _ := identity.Generate()

	changes := map[string]func(*post){
		"another version": func(p *post) { p.Version = 2 },
		"another board":   func(p *post) { p.Board = strings.Repeat("ab", 32) },
		"another author":  func(p *post) { p.Author = other.EncodePublicKey() },
		"a changed body":  func(p *post) { p.Body = "something else" },
		"an empty body":   func(p *post) { p.Body = "   " },
		"a changed nonce": func(p *post) { p.Nonce = strings.Repeat("cd", 16) },
		"a changed id":    func(p *post) { p.ID = strings.Repeat("ef", 32) },
		"no signature":    func(p *post) { p.Signature = "" },
		"a long body":     func(p *post) { p.Body = strings.Repeat("x", MaxBodyBytes+1) },
	}

	for name, change := range changes {
		held := sign(t, me, s.board, "the body", nil)
		change(held)

		if _, err := send(t, s, me.EncodePublicKey(), map[string]any{"op": "post", "post": held}); err == nil {
			t.Errorf("%s: the post landed", name)
		}
	}

	// A post that another citizen signed does not land under this caller.
	held := sign(t, other, s.board, "the body", nil)
	if _, err := send(t, s, me.EncodePublicKey(), map[string]any{"op": "post", "post": held}); err == nil {
		t.Error("a post of another author landed under this caller")
	}
}

func TestAPostOutOfItsTimeIsRefused(t *testing.T) {
	s, me := testBoard(t)

	held := sign(t, me, s.board, "from the past", nil)
	held.CreatedAt = time.Now().Unix() - FreshnessSeconds - 60

	// The signature covers the time, so it is signed again.
	message, _ := signatureMessage(held)
	signature := me.Sign([]byte(message))
	held.Signature = hex.EncodeToString(signature)
	held.ID = sum(append([]byte(message), signature...))

	if _, err := send(t, s, held.Author, map[string]any{"op": "post", "post": held}); err == nil {
		t.Error("a post from outside the window landed")
	}
}

func TestTheBoardHoldsItsLimit(t *testing.T) {
	s, me := testBoard(t)
	s.store.maxPosts = 1

	publish(t, s, me, "the first", nil)

	held := sign(t, me, s.board, "the second", nil)
	if _, err := send(t, s, held.Author, map[string]any{"op": "post", "post": held}); err == nil {
		t.Error("a post over the limit landed")
	}
}

// A post file without a record is taken into the order again.
func TestAPostFileWithoutARecordIsRecovered(t *testing.T) {
	s, me := testBoard(t)
	publish(t, s, me, "the first", nil)

	second := sign(t, me, s.board, "written by hand", nil)
	data, _ := json.Marshal(second)
	if err := os.WriteFile(s.store.postPath(second.ID), data, 0o600); err != nil {
		t.Fatal(err)
	}

	feed, err := send(t, s, me.EncodePublicKey(), map[string]any{"op": "feed"})
	if err != nil {
		t.Fatal(err)
	}
	if posts, _ := feed["posts"].([]any); len(posts) != 2 {
		t.Errorf("the feed holds %d posts after the repair", len(posts))
	}

	// The repair is saved.
	state := &stateFile{}
	data, err = os.ReadFile(s.store.statePath())
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(data, state)
	if len(state.Records) != 2 {
		t.Errorf("the state holds %d records", len(state.Records))
	}
}

func TestABrokenBoardIsRefused(t *testing.T) {
	s, me := testBoard(t)
	publish(t, s, me, "the first", nil)

	if err := os.WriteFile(filepath.Join(s.store.postsDir(), "nonsense.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := send(t, s, me.EncodePublicKey(), map[string]any{"op": "feed"}); err == nil {
		t.Error("a board with a file that does not belong passed")
	}
}

func TestARequestThatIsNotOne(t *testing.T) {
	s, me := testBoard(t)
	from := me.EncodePublicKey()

	for _, body := range []map[string]any{
		{"op": "delete"},
		{"op": "post"},
		{"op": "read", "id": "short"},
		{"op": "thread", "id": "short"},
		{"op": "feed", "limit": 0},
		{"op": "feed", "limit": MaxLimit + 1},
		{"op": "feed", "after": 0},
		{},
	} {
		if _, err := send(t, s, from, body); err == nil {
			t.Errorf("%v passed", body)
		}
	}
}

func TestOneServerHoldsTheBoard(t *testing.T) {
	s, _ := testBoard(t)

	if err := s.store.lock(); err != nil {
		t.Fatal(err)
	}
	defer s.store.unlock()

	other := &store{root: s.store.root, board: s.board, maxPosts: DefaultMaxPosts}
	if err := other.lock(); err == nil {
		t.Error("two servers hold one board")
	}

	// A server that ends lets the next one take the board.
	s.store.unlock()
	if err := other.lock(); err != nil {
		t.Fatalf("the board was not taken after the first server ended: %v", err)
	}
	other.unlock()
}

// A server that crashes holds no lock: the operating system releases it with
// the process. Closing the file without an unlock stands for that.
func TestACrashLeavesNoLock(t *testing.T) {
	s, _ := testBoard(t)

	if err := s.store.lock(); err != nil {
		t.Fatal(err)
	}
	s.store.held.Close()
	s.store.held = nil

	other := &store{root: s.store.root, board: s.board, maxPosts: DefaultMaxPosts}
	if err := other.lock(); err != nil {
		t.Fatalf("the lock of a crashed server was not released: %v", err)
	}
	other.unlock()
}

// Earlier versions held the board with a directory and a process id. The id
// can name a live process in a new container, so the directory never counts.
func TestTheLockDirectoryOfAnEarlierVersionIsTakenOver(t *testing.T) {
	s, _ := testBoard(t)

	if err := os.MkdirAll(s.store.lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	pid := []byte(strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(filepath.Join(s.store.lockPath(), "pid"), pid, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.store.lock(); err != nil {
		t.Fatalf("the old lock directory stopped the board: %v", err)
	}
	s.store.unlock()
}
