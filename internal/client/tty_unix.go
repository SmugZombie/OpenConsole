//go:build !windows

package client

import "os"

// enableVirtualTerminal is a no-op outside Windows.
//
// A Unix terminal has understood the escape sequences a shared shell emits for
// as long as there have been Unix terminals; there is no mode to turn on.
func enableVirtualTerminal(*os.File) (restore func()) { return func() {} }
