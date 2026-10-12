package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/gezibash/arc/sdk/atomicfile"
	wire "github.com/gezibash/arc/sdk/storage"
)

// The rules of admission. STORAGE_ADMIT chooses one.
const (
	// RuleInvite admits a join with a good invite token. It is the default.
	RuleInvite = "invite"
	// RuleAsk keeps the request, and the operator decides.
	RuleAsk = "ask"
	// RuleOpen admits each join at a fixed quota, until the sum of the
	// quotas passes the capacity.
	RuleOpen = "open"
)

const (
	// InviteAge is how long an invite token is good.
	InviteAge = 7 * 24 * time.Hour
	// RequestAge is how long a request waits for the operator under the
	// rule ask.
	RequestAge = 30 * 24 * time.Hour
	// ReadAge is how long a closing account keeps its reads.
	ReadAge = 30 * 24 * time.Hour
	// DefaultPlan is the plan of an account that an invite, the operator
	// or the rule open admits, if no plan is given.
	DefaultPlan = "free"
	// OwnerPlan is the plan of an account key in STORAGE_OWNERS that has no
	// record.
	OwnerPlan = "owner"
)

// The statuses of a record.
const (
	statusAdmitted = wire.StatusAdmitted
	statusPending  = wire.StatusPending
	statusRefused  = "refused"
)

// record is the admission of one account, in its state.json. An account
// key in STORAGE_OWNERS with no record is admitted at the default quota.
type record struct {
	// Status is admitted, pending or refused.
	Status  string `json:"status"`
	Quota   int64  `json:"quota,omitempty"`
	Plan    string `json:"plan,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	// Closing is the time at which the operator closed the account, in
	// Unix seconds, or 0.
	Closing int64 `json:"closing,omitempty"`
	// The request of a join under the rule ask.
	Want    int64  `json:"want,omitempty"`
	Note    string `json:"note,omitempty"`
	Citizen string `json:"citizen,omitempty"`
	Asked   int64  `json:"asked,omitempty"`
}

// standing is what the record of an account allows now.
type standing struct {
	admitted bool
	quota    int64
	plan     string
	expires  int64
	state    string
	// readsEnd is the end of the reads of a closing account, in Unix
	// seconds.
	readsEnd int64
}

// standing reads the record of an account at the time now.
func (s *store) standing(name string, a *account) standing {
	v := s.admission(name, a)
	if v.admitted && v.state == wire.StateOpen && a.Draining > 0 {
		v.state = wire.StateDraining
	}
	return v
}

// admission reads the record of an account at the time now, with no
// drain.
func (s *store) admission(name string, a *account) standing {
	r := a.Record
	if r == nil || r.Status != statusAdmitted {
		if slices.Contains(s.owners, name) {
			return standing{admitted: true, quota: s.quota, plan: OwnerPlan, state: wire.StateOpen}
		}
		return standing{}
	}
	v := standing{admitted: true, quota: r.Quota, plan: r.Plan, expires: r.Expires, state: wire.StateOpen}
	start := r.Closing
	if r.Expires > 0 && (start == 0 || r.Expires < start) {
		start = r.Expires
	}
	if now := s.now().Unix(); start > 0 && now >= start {
		v.state, v.readsEnd = wire.StateClosing, start+int64(ReadAge/time.Second)
		if now >= v.readsEnd {
			v.state = wire.StateClosed
		}
	}
	return v
}

// active says if an account takes a session: it is admitted and not
// closed.
func (v standing) active() bool { return v.admitted && v.state != wire.StateClosed }

func (v standing) account(used int64) *wire.Account {
	return &wire.Account{
		Status: wire.StatusAdmitted, Quota: v.quota, Used: used, Plan: v.plan, State: v.state, Expires: v.expires,
		TombstoneDays: int(TombstoneAge / (24 * time.Hour)), ReadDaysAfterClose: int(ReadAge / (24 * time.Hour)),
	}
}

// active says if the account of name takes a session.
func (s *store) active(name string) (bool, error) {
	a, err := s.load(name)
	if err != nil {
		return false, err
	}
	return s.standing(name, a).active(), nil
}

// names returns each account that STORAGE_OWNERS names or that has a
// directory.
func (s *store) names() ([]string, error) {
	names := slices.Clone(s.owners)
	entries, err := os.ReadDir(filepath.Join(s.dir, "accounts"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() && wire.IsHex64(e.Name()) && !slices.Contains(names, e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// invite is one invite token, kept by the SHA-256 of the token.
type invite struct {
	Quota   int64  `json:"quota"`
	Plan    string `json:"plan"`
	Expires int64  `json:"expires"`
}

func (s *store) admissionPath() string { return filepath.Join(s.dir, "invites.json") }

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// invites reads the invite tokens, without the old ones.
func (s *store) invites() (map[string]invite, error) {
	invites := map[string]invite{}
	raw, err := os.ReadFile(s.admissionPath())
	if errors.Is(err, os.ErrNotExist) {
		return invites, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &invites); err != nil {
		return nil, fmt.Errorf("%s: %w", s.admissionPath(), err)
	}
	now := s.now().Unix()
	for hash, inv := range invites {
		if now >= inv.Expires {
			delete(invites, hash)
		}
	}
	return invites, nil
}

func (s *store) writeInvites(invites map[string]invite) error {
	raw, err := json.Marshal(invites)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.admissionPath(), raw, 0o600)
}

// rules are the settings of admission.
type rules struct {
	rule      string
	openQuota int64
	capacity  int64
}

// join answers a join of the account key name. The lock of invites.json
// covers each join, then the lock of the account. A good token is spent
// first, and the account is admitted after: if the program stops between
// the two, the token is lost, and no token admits twice.
func (s *store) join(name string, req wire.Join, r rules) (wire.JoinReply, error) {
	unlock, err := atomicfile.Lock(s.admissionPath())
	if err != nil {
		return wire.JoinReply{}, err
	}
	defer unlock()
	a, err := s.load(name)
	if err != nil {
		return wire.JoinReply{}, err
	}
	if v := s.standing(name, a); v.admitted {
		used, err := s.used(name)
		if err != nil {
			return wire.JoinReply{}, err
		}
		return wire.JoinReply{Version: 1, Account: v.account(used)}, nil
	}
	now := s.now().Unix()
	if req.Invite != "" {
		invites, err := s.invites()
		if err != nil {
			return wire.JoinReply{}, err
		}
		inv, ok := invites[tokenHash(req.Invite)]
		if !ok {
			return refused("the invite token is unknown, spent, or older than 7 days"), nil
		}
		delete(invites, tokenHash(req.Invite))
		if err := s.writeInvites(invites); err != nil {
			return wire.JoinReply{}, err
		}
		return s.admitAccount(name, inv.Quota, inv.Plan, 0)
	}
	switch r.rule {
	case RuleAsk:
		var reply wire.JoinReply
		err := s.change(name, func(a *account) (bool, error) {
			if a.Record != nil && a.Record.Status == statusRefused {
				reply = refused("the operator refused this account")
				return false, nil
			}
			reply = wire.JoinReply{Version: 1, Account: &wire.Account{Status: wire.StatusPending, Message: "the operator decides by hand; ask again later"}}
			if a.Record != nil && a.Record.Status == statusPending && now < a.Record.Asked+int64(RequestAge/time.Second) {
				return false, nil
			}
			a.Record = &record{Status: statusPending, Want: req.Want, Note: req.Note, Citizen: req.Citizen, Asked: now}
			return true, nil
		})
		return reply, err
	case RuleOpen:
		sum, err := s.quotas()
		if err != nil {
			return wire.JoinReply{}, err
		}
		if sum+r.openQuota > r.capacity {
			return refused(fmt.Sprintf("the storage citizen is full: its accounts hold quotas of %d of %d bytes", sum, r.capacity)), nil
		}
		return s.admitAccount(name, r.openQuota, DefaultPlan, 0)
	}
	return refused("this storage citizen admits only with an invite"), nil
}

func refused(message string) wire.JoinReply {
	return wire.JoinReply{Version: 1, Error: wire.ErrRefused, Message: message}
}

// admitAccount makes the record of an account admitted, and keeps its blobs.
func (s *store) admitAccount(name string, quota int64, plan string, expires int64) (wire.JoinReply, error) {
	used, err := s.used(name)
	if err != nil {
		return wire.JoinReply{}, err
	}
	var reply wire.JoinReply
	err = s.change(name, func(a *account) (bool, error) {
		a.Record = &record{Status: statusAdmitted, Quota: quota, Plan: plan, Expires: expires}
		reply = wire.JoinReply{Version: 1, Account: s.standing(name, a).account(used)}
		return true, nil
	})
	return reply, err
}

// quotas returns the sum of the quotas of the accounts that are not
// closed.
func (s *store) quotas() (int64, error) {
	names, err := s.names()
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, name := range names {
		a, err := s.load(name)
		if err != nil {
			return 0, err
		}
		if v := s.standing(name, a); v.active() {
			sum += v.quota
		}
	}
	return sum, nil
}

// Operator changes the accounts and the invites on the disk of a storage
// citizen. It takes the same locks as the service, so it runs while the
// service runs.
type Operator struct{ s *store }

// NewOperator works on the storage directory dir.
func NewOperator(dir string) *Operator {
	return &Operator{s: &store{dir: dir, now: time.Now}}
}

// Invite makes an invite token for an account with quota and plan. The
// token is good once, for InviteAge.
func (o *Operator) Invite(quota int64, plan string) (string, error) {
	if quota <= 0 {
		return "", errors.New("the quota of an invite must be more than 0 bytes")
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	unlock, err := atomicfile.Lock(o.s.admissionPath())
	if err != nil {
		return "", err
	}
	defer unlock()
	invites, err := o.s.invites()
	if err != nil {
		return "", err
	}
	invites[tokenHash(token)] = invite{Quota: quota, Plan: plan, Expires: o.s.now().Add(InviteAge).Unix()}
	return token, o.s.writeInvites(invites)
}

// Request is one request that waits for the operator.
type Request struct {
	Account string
	Want    int64
	Note    string
	Citizen string
	Asked   time.Time
}

// Requests returns the requests that wait, the oldest first.
func (o *Operator) Requests() ([]Request, error) {
	names, err := o.s.names()
	if err != nil {
		return nil, err
	}
	var out []Request
	now := o.s.now().Unix()
	for _, name := range names {
		a, err := o.s.load(name)
		if err != nil {
			return nil, err
		}
		r := a.Record
		if r == nil || r.Status != statusPending || now >= r.Asked+int64(RequestAge/time.Second) {
			continue
		}
		out = append(out, Request{Account: name, Want: r.Want, Note: r.Note, Citizen: r.Citizen, Asked: time.Unix(r.Asked, 0)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asked.Before(out[j].Asked) })
	return out, nil
}

// Accept admits an account at quota and plan. The account need not have
// asked: the operator can admit an account key that it got by hand.
func (o *Operator) Accept(name string, quota int64, plan string) error {
	if quota <= 0 {
		return errors.New("the quota of an account must be more than 0 bytes")
	}
	return o.s.change(name, func(a *account) (bool, error) {
		if a.Record != nil && a.Record.Status == statusAdmitted {
			return false, fmt.Errorf("%s is admitted already; change it with arc-storage account set", name)
		}
		a.Record = &record{Status: statusAdmitted, Quota: quota, Plan: plan}
		return true, nil
	})
}

// Refuse refuses the request of an account. The next join gets refused.
func (o *Operator) Refuse(name string) error {
	return o.s.change(name, func(a *account) (bool, error) {
		if a.Record != nil && a.Record.Status == statusAdmitted {
			return false, fmt.Errorf("%s is admitted; close it with arc-storage account close", name)
		}
		if a.Record == nil {
			a.Record = &record{}
		}
		a.Record.Status = statusRefused
		return true, nil
	})
}

// Change are the values that Set changes. A nil value stays.
type Change struct {
	Quota   *int64
	Plan    *string
	Expires *int64
}

// Set changes the record of an admitted account. An account key of
// STORAGE_OWNERS with no record gets one, with the plan owner; owners
// gives these keys and quota their default quota.
func (o *Operator) Set(name string, owners []string, quota int64, c Change) error {
	return o.s.change(name, func(a *account) (bool, error) {
		if err := o.record(name, a, owners, quota); err != nil {
			return false, err
		}
		if c.Quota != nil {
			a.Record.Quota = *c.Quota
		}
		if c.Plan != nil {
			a.Record.Plan = *c.Plan
		}
		if c.Expires != nil {
			a.Record.Expires = *c.Expires
		}
		return true, nil
	})
}

// Close starts the end of an account: it refuses each put, and keeps its
// reads for ReadAge.
func (o *Operator) Close(name string, owners []string, quota int64) error {
	return o.s.change(name, func(a *account) (bool, error) {
		if err := o.record(name, a, owners, quota); err != nil {
			return false, err
		}
		if a.Record.Closing == 0 {
			a.Record.Closing = o.s.now().Unix()
		}
		return true, nil
	})
}

func (o *Operator) record(name string, a *account, owners []string, quota int64) error {
	if a.Record != nil && a.Record.Status == statusAdmitted {
		return nil
	}
	if !slices.Contains(owners, name) {
		return fmt.Errorf("%s is not an admitted account", name)
	}
	a.Record = &record{Status: statusAdmitted, Quota: quota, Plan: OwnerPlan}
	return nil
}
