package bms

import (
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// allowAll admits every client to every topic on the twin-hosted broker, which listens on the
// substrate's loopback port only. (mochi's own hooks/auth is not used: it pulls in a YAML
// dependency for its ledger that this module does not otherwise require.)
type allowAll struct{ mqtt.HookBase }

func (h *allowAll) ID() string { return "dc-twin-bms-allow-all" }

func (h *allowAll) Provides(b byte) bool {
	return b == mqtt.OnConnectAuthenticate || b == mqtt.OnACLCheck
}

func (h *allowAll) OnConnectAuthenticate(*mqtt.Client, packets.Packet) bool { return true }

func (h *allowAll) OnACLCheck(*mqtt.Client, string, bool) bool { return true }

// closeEmbedded stops the twin-hosted broker. MQTT 3.1.1 has no DISCONNECT from the server
// (§3.14: it is sent by the client; a 3.1.1 server that ends a session closes the connection), but
// mochi's Close writes one to every client, which a 3.1.1 client reports as a protocol error —
// rumqttc under nico-api logs "Invalid incoming packet = Disconnect" and an event-loop connection
// error when the twin goes down. Its 3.1.1 connections are closed first, so mochi's DISCONNECT
// reaches only MQTT 5 clients, for whom it is the protocol's.
func closeEmbedded(srv *mqtt.Server) {
	for _, cl := range srv.Clients.GetAll() {
		if cl.Properties.ProtocolVersion < 5 {
			cl.Stop(packets.ErrServerShuttingDown)
		}
	}
	_ = srv.Close()
}
