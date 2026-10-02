// Package deploy holds the files that install the native-ops daemon on a host, embedded so the binary
// can write them itself (a new host's cloud-init, see engine.DaemonInstall) from the same source the
// repository ships.
package deploy

import _ "embed"

// ServeUnit is the systemd unit for `native-ops serve` (deploy/systemd/native-ops-serve.service).
//
//go:embed systemd/native-ops-serve.service
var ServeUnit string
