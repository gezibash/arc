package iface_test

import (
	"strings"
	"testing"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/iface"
)

func TestServiceDeclaresSupportedInteractions(t *testing.T) {
	manifest := `{"interface":1,"id":"repl","shape":"service","service":{"method":"EVAL","path":"/"},"kinds":{},"formats":{},"commands":[]}`
	unary := strings.Replace(manifest, `"commands":[]`, `"commands":[{"path":["run"],"summary":"Run a query","action":{"call":{"class":"live","body":"query"}}}]`, 1)
	if _, err := iface.Parse([]byte(manifest)); err == nil {
		t.Fatal("an ordinary service with no commands was accepted")
	}
	plain, err := iface.Parse([]byte(unary))
	if err != nil {
		t.Fatal(err)
	}
	if !session.Supports(plain.Service.Interactions, session.RequestReply) || session.Supports(plain.Service.Interactions, session.Duplex) {
		t.Fatal("omitted interactions must mean request/reply only")
	}
	for _, declaration := range []struct {
		modes string
		valid bool
	}{
		{`["request_reply","server_stream","duplex"]`, true},
		{`["duplex"]`, true}, {`["unknown"]`, false}, {`["duplex","duplex"]`, false},
	} {
		body := strings.Replace(manifest, `"method":"EVAL"`, `"interactions":`+declaration.modes+`,"method":"EVAL"`, 1)
		parsed, err := iface.Parse([]byte(body))
		if (err == nil) != declaration.valid {
			t.Fatalf("%s: %v", declaration.modes, err)
		}
		if err == nil && !session.Supports(parsed.Service.Interactions, session.Duplex) {
			t.Fatal("declared duplex is unavailable")
		}
	}
}
