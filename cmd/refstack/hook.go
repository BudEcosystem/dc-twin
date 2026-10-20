package main

import (
	"bytes"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// allowAll lets every client connect and publish: the reference stack's broker is a test
// broker, not DSX Exchange's auth callout.
type allowAll struct{ mochi.HookBase }

func (h *allowAll) ID() string { return "allow-all" }
func (h *allowAll) Provides(b byte) bool {
	return bytes.Contains([]byte{mochi.OnConnectAuthenticate, mochi.OnACLCheck}, []byte{b})
}
func (h *allowAll) OnConnectAuthenticate(cl *mochi.Client, pk packets.Packet) bool { return true }
func (h *allowAll) OnACLCheck(cl *mochi.Client, topic string, write bool) bool     { return true }
