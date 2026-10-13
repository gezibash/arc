package citizen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/runtime/selfsync"
	"github.com/gezibash/arc/sdk/storage"
)

// ErrOlderStorage says that a storage citizen answered a join with
// invalid_request: it runs a storage app from before admission.
var ErrOlderStorage = errors.New("it runs an older storage app: it cannot admit accounts; use STORAGE_OWNERS")

// JoinStorage asks a storage citizen for an account. The account key of
// this citizen for that storage citizen authors the call. With
// showCitizen, the join also carries the citizen key and its signature
// over the account key and the key of the storage citizen. The reply is an
// account, admitted or pending, or the error refused.
func (sess *Session) JoinStorage(ctx context.Context, provider nostr.PubKey, join storage.Join, showCitizen bool) (storage.JoinReply, error) {
	if err := sess.RequireSigner(); err != nil {
		return storage.JoinReply{}, err
	}
	account, err := selfsync.AccountKey(sess.Key.Secret, provider)
	if err != nil {
		return storage.JoinReply{}, err
	}
	if showCitizen {
		join.Citizen = sess.Key.Public.Hex()
		if join.Sig, err = storage.SignJoin(sess.Key.Secret, account.Public, provider); err != nil {
			return storage.JoinReply{}, err
		}
	}
	body, err := json.Marshal(storage.JoinCall{Version: 1, Join: join})
	if err != nil {
		return storage.JoinReply{}, err
	}
	request := call.Request{Capability: StorageApp, Method: "RAW", Path: "/", Body: string(body)}
	reply, _, _, err := sess.LiveCallAs(ctx, account, provider, request, call.Timeout)
	if err != nil {
		return storage.JoinReply{}, err
	}
	if strings.Contains(reply.Err, storage.ErrInvalidRequest) {
		return storage.JoinReply{}, ErrOlderStorage
	}
	if reply.Err != "" {
		return storage.JoinReply{}, errors.New(reply.Err)
	}
	var out storage.JoinReply
	if json.Unmarshal([]byte(reply.Body), &out) != nil || out.Version != 1 || (out.Account == nil) == (out.Error == "") {
		return storage.JoinReply{}, fmt.Errorf("the storage citizen sent a reply that is not a join reply: %.200s", reply.Body)
	}
	return out, nil
}

// errOlderClose says that a storage citizen answered a close with
// invalid_request: it runs a storage app from before the close call.
var errOlderClose = errors.New("it runs an older storage app, which keeps the empty account; update it to close the account")

// CloseStorage ends the account of this citizen on a storage citizen: the
// citizen leaves it. The account key authors the call. The storage citizen
// removes an empty account at once, and refuses an account that holds a
// blob. A later join asks for a new account.
func (sess *Session) CloseStorage(ctx context.Context, provider nostr.PubKey) (storage.CloseReply, error) {
	if err := sess.RequireSigner(); err != nil {
		return storage.CloseReply{}, err
	}
	account, err := selfsync.AccountKey(sess.Key.Secret, provider)
	if err != nil {
		return storage.CloseReply{}, err
	}
	body, err := json.Marshal(storage.CloseCall{Version: 1})
	if err != nil {
		return storage.CloseReply{}, err
	}
	request := call.Request{Capability: StorageApp, Method: "RAW", Path: "/", Body: string(body)}
	reply, _, _, err := sess.LiveCallAs(ctx, account, provider, request, call.Timeout)
	if err != nil {
		return storage.CloseReply{}, err
	}
	if strings.Contains(reply.Err, storage.ErrInvalidRequest) {
		return storage.CloseReply{}, errOlderClose
	}
	if reply.Err != "" {
		return storage.CloseReply{}, errors.New(reply.Err)
	}
	var out storage.CloseReply
	if json.Unmarshal([]byte(reply.Body), &out) != nil || out.Version != 1 || out.Closed == (out.Error != "") {
		return storage.CloseReply{}, fmt.Errorf("the storage citizen sent a reply that is not a close reply: %.200s", reply.Body)
	}
	return out, nil
}
