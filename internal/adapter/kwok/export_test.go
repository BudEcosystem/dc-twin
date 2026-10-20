package kwok

import "time"

// SetRenewEvery shortens the lease renewal interval for a test; it returns the old value.
func SetRenewEvery(d time.Duration) time.Duration {
	old := renewEvery
	renewEvery = d
	return old
}
