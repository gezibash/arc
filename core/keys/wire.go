package keys

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"fiatjaf.com/nostr"
)

// Request is one request to a signer: a NIP-46 request object.
type Request struct {
	ID     string   `json:"id"`
	Method string   `json:"method"`
	Params []string `json:"params"`
}

// Response is the answer of a signer: a NIP-46 response object.
type Response struct {
	ID     string `json:"id"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// The holder requests of docs/devices/SPEC.md, section 3.7.
const (
	MethodKeyedRoot   = "arc_keyed_root"
	MethodAccountKey  = "arc_account_key"
	MethodSignJoin    = "arc_sign_join"
	MethodSignRelease = "arc_sign_release"
	MethodSignChange  = "arc_sign_change"
)

// HolderMethod says whether method is a holder request. A bunker refuses
// each one over a relay.
func HolderMethod(method string) bool {
	switch method {
	case MethodKeyedRoot, MethodAccountKey, MethodSignJoin, MethodSignRelease, MethodSignChange:
		return true
	}
	return false
}

// Answer answers one request with this key: the NIP-46 requests that ARC
// uses, and the holder requests.
func (k Key) Answer(ctx context.Context, req Request) Response {
	result, err := k.answer(ctx, req)
	if err != nil {
		return Response{ID: req.ID, Error: err.Error()}
	}
	return Response{ID: req.ID, Result: result}
}

func (k Key) answer(ctx context.Context, req Request) (string, error) {
	param := func(i int) (string, error) {
		if i >= len(req.Params) {
			return "", fmt.Errorf("%s needs %d params", req.Method, i+1)
		}
		return req.Params[i], nil
	}
	key := func(i int) (nostr.PubKey, error) {
		text, err := param(i)
		if err != nil {
			return nostr.PubKey{}, err
		}
		return nostr.PubKeyFromHex(text)
	}
	switch req.Method {
	case "ping":
		return "pong", nil
	case "get_public_key":
		return k.Public.Hex(), nil
	case "sign_event":
		text, err := param(0)
		if err != nil {
			return "", err
		}
		var event nostr.Event
		if err := event.UnmarshalJSON([]byte(text)); err != nil {
			return "", fmt.Errorf("sign_event: %w", err)
		}
		if err := k.SignEvent(ctx, &event); err != nil {
			return "", err
		}
		return event.String(), nil
	case "nip44_encrypt", "nip44_decrypt", "nip04_encrypt", "nip04_decrypt":
		peer, err := key(0)
		if err != nil {
			return "", err
		}
		text, err := param(1)
		if err != nil {
			return "", err
		}
		switch req.Method {
		case "nip44_encrypt":
			return k.Encrypt(ctx, text, peer)
		case "nip44_decrypt":
			return k.Decrypt(ctx, text, peer)
		case "nip04_encrypt":
			return k.Nip04Encrypt(ctx, text, peer)
		default:
			return k.Nip04Decrypt(ctx, text, peer)
		}
	case MethodKeyedRoot:
		root, err := k.KeyedRoot(ctx)
		return hex.EncodeToString(root), err
	case MethodAccountKey:
		storage, err := key(0)
		if err != nil {
			return "", err
		}
		account, err := k.AccountKey(ctx, storage)
		return account.Secret.Hex(), err
	case MethodSignJoin:
		account, err := key(0)
		if err != nil {
			return "", err
		}
		storage, err := key(1)
		if err != nil {
			return "", err
		}
		return k.SignJoin(ctx, account, storage)
	case MethodSignRelease:
		text, err := param(0)
		if err != nil {
			return "", err
		}
		schema, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return "", errors.New("arc_sign_release: the schema is not a number")
		}
		text, err = param(1)
		if err != nil {
			return "", err
		}
		encoded, err := hex.DecodeString(text)
		if err != nil {
			return "", errors.New("arc_sign_release: the bytes are not hex")
		}
		return k.SignRelease(ctx, schema, encoded)
	case MethodSignChange:
		blob, err := param(0)
		if err != nil {
			return "", err
		}
		text, err := param(1)
		if err != nil {
			return "", err
		}
		record, err := nostr.IDFromHex(text)
		if err != nil {
			return "", errors.New("arc_sign_change: the record ID is not hex")
		}
		return k.SignChange(ctx, blob, record)
	}
	return "", fmt.Errorf("the signer does not answer %q", req.Method)
}

// Remote is a citizen key on a signer. Each method is one request through
// Call. It is a keys.Signer, and answers the holder requests too.
type Remote struct {
	Public nostr.PubKey
	// Call sends one request to the signer, and returns its result.
	Call func(ctx context.Context, method string, params ...string) (string, error)
}

// PublicKey is the citizen key.
func (r Remote) PublicKey() nostr.PubKey { return r.Public }

// GetPublicKey is the citizen key.
func (r Remote) GetPublicKey(ctx context.Context) (nostr.PubKey, error) { return r.Public, ctx.Err() }

// SignEvent asks the signer to sign event, and checks the signature.
func (r Remote) SignEvent(ctx context.Context, event *nostr.Event) error {
	result, err := r.Call(ctx, "sign_event", event.String())
	if err != nil {
		return err
	}
	var signed nostr.Event
	if err := signed.UnmarshalJSON([]byte(result)); err != nil {
		return fmt.Errorf("the signer gave no event: %w", err)
	}
	if signed.PubKey != r.Public || !signed.CheckID() || !signed.VerifySignature() {
		return errors.New("the signer gave an event with a bad signature")
	}
	*event = signed
	return nil
}

// Encrypt is NIP-44 through the signer.
func (r Remote) Encrypt(ctx context.Context, plaintext string, to nostr.PubKey) (string, error) {
	return r.Call(ctx, "nip44_encrypt", to.Hex(), plaintext)
}

// Decrypt is NIP-44 through the signer.
func (r Remote) Decrypt(ctx context.Context, ciphertext string, from nostr.PubKey) (string, error) {
	return r.Call(ctx, "nip44_decrypt", from.Hex(), ciphertext)
}

// Nip04Encrypt is NIP-04 through the signer.
func (r Remote) Nip04Encrypt(ctx context.Context, plaintext string, to nostr.PubKey) (string, error) {
	return r.Call(ctx, "nip04_encrypt", to.Hex(), plaintext)
}

// Nip04Decrypt is NIP-04 through the signer.
func (r Remote) Nip04Decrypt(ctx context.Context, ciphertext string, from nostr.PubKey) (string, error) {
	return r.Call(ctx, "nip04_decrypt", from.Hex(), ciphertext)
}

// KeyedRoot asks the signer for the keyed root.
func (r Remote) KeyedRoot(ctx context.Context) ([]byte, error) {
	result, err := r.Call(ctx, MethodKeyedRoot)
	if err != nil {
		return nil, err
	}
	root, err := hex.DecodeString(result)
	if err != nil || len(root) != 32 {
		return nil, errors.New("the signer gave a keyed root that is not 32 bytes")
	}
	return root, nil
}

// AccountKey asks the signer for the account key for one storage citizen.
func (r Remote) AccountKey(ctx context.Context, storage nostr.PubKey) (Key, error) {
	result, err := r.Call(ctx, MethodAccountKey, storage.Hex())
	if err != nil {
		return Key{}, err
	}
	secret, err := nostr.SecretKeyFromHex(result)
	if err != nil {
		return Key{}, errors.New("the signer gave an account key that is not a key")
	}
	return FromSecret(secret), nil
}

// SignJoin asks the signer to sign the join digest.
func (r Remote) SignJoin(ctx context.Context, account, storage nostr.PubKey) (string, error) {
	return r.Call(ctx, MethodSignJoin, account.Hex(), storage.Hex())
}

// SignRelease asks the signer to sign the release digest.
func (r Remote) SignRelease(ctx context.Context, schema int64, encoded []byte) (string, error) {
	return r.Call(ctx, MethodSignRelease, strconv.FormatInt(schema, 10), hex.EncodeToString(encoded))
}

// SignChange asks the signer to sign the change digest of a blob.
func (r Remote) SignChange(ctx context.Context, blob string, record nostr.ID) (string, error) {
	return r.Call(ctx, MethodSignChange, blob, record.Hex())
}
