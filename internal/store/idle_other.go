//go:build !(linux || darwin)

package store

import "net"

// idleNews cannot look at a connection here without reading from it, so
// it reports false, and the pool's ping of a connection idle for more
// than a second is all the check there is.
func idleNews(net.Conn) bool { return false }
