package mail

import "time"

// SetNow sets the clock of the mail, for tests.
func SetNow(m *Mail, now func() time.Time) { m.now = now }

// SetWork sets the difficulty that the mail makes and asks of strangers,
// so that tests that are not about the constant search fewer bits.
func SetWork(m *Mail, bits int) { m.work = bits }

// SetMaxHeld sets the cap of the held mail, so that a test fills it with
// few messages.
func SetMaxHeld(m *Mail, n int) { m.maxHeld = n }
