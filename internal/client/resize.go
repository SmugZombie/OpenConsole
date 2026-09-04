package client

// notify reports a size change without blocking.
//
// The channel has room for one pending change and a change is a fact, not a
// queue: if one is already waiting, the reader has not looked yet and will see
// the current size when it does. Blocking here would stall a signal handler or
// a poller for the sake of a number that is about to be re-read anyway.
func notify(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
