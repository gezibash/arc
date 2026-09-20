package citizen_test

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/go/capability"
	"github.com/gezibash/arc/go/citizen"
	"github.com/gezibash/arc/go/client"
	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/relay"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// build compiles the provider that these tests serve.
func build(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "echo-provider")
	command := exec.Command("go", "build", "-o", binary, "./testdata/echo")

	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("the provider did not build: %v: %s", err, output)
	}
	return binary
}

// stack starts a relay, a serving citizen, and a caller.
func stack(t *testing.T) (*citizen.Citizen, *client.Peers) {
	t.Helper()

	relayIdentity, _ := identity.Generate()
	server, err := relay.Listen(context.Background(), relay.Options{
		Identity: relayIdentity,
		Address:  "127.0.0.1:0",
		Log:      quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })

	binary := build(t)
	manifest, err := filepath.Abs(filepath.Join("testdata", "echo", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	me, _ := identity.Generate()
	serving, err := citizen.Serve(context.Background(), citizen.Options{
		Identity:       me,
		Relay:          server.Addr().String(),
		RelayPublicKey: server.PublicKey(),
		Serve:          "exec://" + binary + "?manifest=" + manifest,
		Log:            quiet,
	})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { serving.Close() })

	caller, _ := identity.Generate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connection, err := client.Dial(ctx, server.Addr().String(), client.Options{
		Identity:       caller,
		RelayPublicKey: server.PublicKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })

	return serving, connection.Peers()
}

// The whole stack is Go: the relay, the citizen, the provider and the caller.
func TestACallerReachesTheProvider(t *testing.T) {
	serving, peers := stack(t)
	ctx := context.Background()

	address := "echo+arc://" + identity.Name(serving.PublicKey())[:0] +
		hexOf(serving.PublicKey()) + "/hello"

	answer, err := peers.Call(ctx, address, []byte("body"), "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(answer.Body) != "ECHO /hello body" {
		t.Errorf("body = %q", answer.Body)
	}
}

func TestTheCitizenServesItsManifest(t *testing.T) {
	serving, peers := stack(t)
	ctx := context.Background()

	summary, err := peers.Manifest(ctx, serving.PublicKey())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if summary["view"] != "summary" {
		t.Errorf("summary = %v", summary)
	}

	capabilities := capability.SummaryCapabilities(summary)
	if len(capabilities) != 1 || capabilities[0]["scheme"] != "echo" {
		t.Fatalf("the summary holds %v", capabilities)
	}

	// The detail carries the signature of the citizen, and the caller checks it.
	detail, err := peers.Detail(ctx, serving.PublicKey(), "primary")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}

	fields, _ := detail["capability"].(map[string]any)
	if fields["title"] != "Echo over ARC" {
		t.Errorf("detail = %v", fields)
	}

	if _, err := peers.Detail(ctx, serving.PublicKey(), "missing"); err == nil {
		t.Error("a capability that is not there was served")
	}
}

func TestTheCitizenAnnouncesItself(t *testing.T) {
	serving, peers := stack(t)
	_ = serving

	// The caller uses its own connection to search the directory.
	ctx := context.Background()
	answer, err := peers.Call(ctx, "echo+arc://"+hexOf(serving.PublicKey())+"/", []byte("x"), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(answer.Body), "x") {
		t.Errorf("body = %q", answer.Body)
	}
}

func TestAnErrorOfTheProviderReachesTheCaller(t *testing.T) {
	serving, peers := stack(t)

	_, err := peers.Call(context.Background(), "echo+arc://"+hexOf(serving.PublicKey())+"/", []byte("fail"), "")
	if err == nil {
		t.Fatal("the call passed")
	}

	var remote *client.RemoteError
	if !asRemote(err, &remote) || remote.Code != "provider_error" {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(remote.Message, "refused_on_purpose") {
		t.Errorf("message = %q", remote.Message)
	}
}

func TestAPanicOfTheProviderFailsOneCallOnly(t *testing.T) {
	serving, peers := stack(t)
	ctx := context.Background()
	address := "echo+arc://" + hexOf(serving.PublicKey()) + "/"

	if _, err := peers.Call(ctx, address, []byte("panic"), ""); err == nil {
		t.Error("the call that panicked passed")
	}

	answer, err := peers.Call(ctx, address, []byte("after"), "")
	if err != nil {
		t.Fatalf("the next call failed: %v", err)
	}
	if !strings.HasSuffix(string(answer.Body), "after") {
		t.Errorf("body = %q", answer.Body)
	}
}

func TestRefusesAnAddressThatIsNotOne(t *testing.T) {
	for _, address := range []string{
		"http://example.com/",
		"exec+arc://not-hex/",
		"exec+arc://" + strings.Repeat("a", 63) + "/",
		"arc://" + strings.Repeat("a", 64) + "/",
		"exec+arc://" + strings.Repeat("a", 64) + ":8080/",
		"exec+arc://" + strings.Repeat("a", 64) + "/?q=1",
	} {
		if _, err := client.ParseAddress(address); err == nil {
			t.Errorf("%s passed", address)
		}
	}

	parsed, err := client.ParseAddress("exec+arc://" + strings.Repeat("ab", 32) + "/run/now")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "exec" || parsed.Path != "/run/now" || len(parsed.Key) != 32 {
		t.Errorf("address = %+v", parsed)
	}
}

func TestParseServeURI(t *testing.T) {
	binary := build(t)
	manifest, _ := filepath.Abs(filepath.Join("testdata", "echo", "manifest.json"))

	path, _, found, err := citizen.ParseServeURI("exec://" + binary + "?manifest=" + manifest)
	if err != nil || path != binary || found != manifest {
		t.Fatalf("parse gave %q %q %v", path, found, err)
	}

	for _, uri := range []string{
		"http://" + binary,
		"exec://" + binary,
		"exec:///does/not/exist?manifest=" + manifest,
	} {
		if _, _, _, err := citizen.ParseServeURI(uri); err == nil {
			t.Errorf("%s passed", uri)
		}
	}
}
