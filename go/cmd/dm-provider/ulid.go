package main

import (
	"crypto/rand"
	"encoding/binary"
	"math/big"
	"regexp"
	"sync"
	"time"
)

// A ULID is 26 characters: 48 bits of milliseconds, 80 random bits, in
// Crockford base32. The ids sort by time. Inside one process, two ids of the
// same millisecond rise.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

var lastID struct {
	sync.Mutex
	at     int64
	random *big.Int
}

func newULID() (string, error) {
	now := time.Now().UnixMilli()

	lastID.Lock()
	defer lastID.Unlock()

	if lastID.at == now && lastID.random != nil {
		lastID.random = new(big.Int).Add(lastID.random, big.NewInt(1))
	} else {
		bytes := make([]byte, 10)
		if _, err := rand.Read(bytes); err != nil {
			return "", err
		}
		lastID.at = now
		lastID.random = new(big.Int).SetBytes(bytes)
	}

	// 48 bits of time, then 80 bits of randomness, as one number of 128 bits.
	value := new(big.Int).SetUint64(uint64(now))
	value.Lsh(value, 80)
	value.Or(value, lastID.random)

	return encodeULID(value), nil
}

// encodeULID writes 128 bits as 26 groups of five bits.
func encodeULID(value *big.Int) string {
	out := make([]byte, 26)
	mask := big.NewInt(31)

	for index := 25; index >= 0; index-- {
		group := new(big.Int).And(value, mask)
		out[index] = alphabet[group.Int64()]
		value = new(big.Int).Rsh(value, 5)
	}
	return string(out)
}

func validULID(id string) bool { return ulidPattern.MatchString(id) }

var _ = binary.BigEndian
