package store

import "context"

// ExecForTest runs a statement on the store's database, for tests that
// set up rows the API cannot make.
func ExecForTest(s Store, q string, args ...any) error {
	_, err := s.(*db).r().exec(context.Background(), q, args...)
	return err
}

// IsDeadlockForTest is isDeadlock.
var IsDeadlockForTest = isDeadlock
