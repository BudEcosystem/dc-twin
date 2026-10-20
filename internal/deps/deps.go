//go:build tools

// Package deps pins libraries adapters and their tests use, so they resolve before the code
// that imports them lands. Nothing imports this package.
package deps

import (
	_ "github.com/bmc-toolbox/bmclib/v2"
	_ "github.com/eclipse/paho.mqtt.golang"
	_ "github.com/gosnmp/gosnmp"
	_ "github.com/insomniacslk/dhcp/dhcpv4"
	_ "github.com/mochi-mqtt/server/v2"
	_ "github.com/nats-io/nats.go"
	_ "github.com/pin/tftp/v3"
	_ "github.com/prometheus/common/expfmt"
	_ "github.com/santhosh-tekuri/jsonschema/v6"
	_ "github.com/stmcginnis/gofish"
	_ "k8s.io/api/core/v1"
	_ "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/kubernetes"
	_ "sigs.k8s.io/controller-runtime/pkg/envtest"
)
