//go:build arctest

package private

// Work is 8 bits in a build with the tag arctest. The tests of the apps
// build the arc program with it, because they run many citizens that write
// to each other for the first time, and they test the apps, not the
// constant. A release build has no such tag. The tests of core/private and
// core/mail prove the constant of 20 bits.
const Work = 8
