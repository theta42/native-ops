package remote

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
)

// MeshProbeScript reads, and only reads, the host's side of a WireGuard mesh (engine.PlanMesh): whether its
// key exists, its public key, the interface's unit, routes and config file, IP forwarding, and the ufw rules
// the mesh added. It is fed on stdin to `sh -s -- <interface>` (MeshProbeCommand), the one script the
// read-only executor lets through, and only byte for byte. It never reads the private key: it only tests
// that the file is there.
const MeshProbeScript = `i="$1"
d=/etc/wireguard
if [ -s "$d/$i.key" ]; then echo "keyfile yes"; else echo "keyfile no"; fi
echo "pub $(cat "$d/$i.pub" 2>/dev/null)"
if command -v wg-quick >/dev/null 2>&1; then echo "tools yes"; else echo "tools no"; fi
echo "active $(systemctl is-active "wg-quick@$i" 2>/dev/null)"
echo "enabled $(systemctl is-enabled "wg-quick@$i" 2>/dev/null)"
echo "livepub $(wg show "$i" public-key 2>/dev/null)"
echo "forward $(sysctl -n net.ipv4.ip_forward 2>/dev/null)"
echo "forwardconf $(cat /etc/sysctl.d/60-native-ops-mesh.conf 2>/dev/null | tr -d '\n')"
echo "ufw $(ufw status 2>/dev/null | head -n 1)"
ip -4 route show dev "$i" 2>/dev/null | sed 's/^/route /'
ufw show added 2>/dev/null | grep -F "comment 'native-ops mesh'" | sed 's/^/rule /'
echo "conf"
cat "$d/$i.conf" 2>/dev/null
exit 0
`

var meshProbeCmdRe = regexp.MustCompile(`^sh -s -- [A-Za-z][A-Za-z0-9_-]{0,14}$`)

// MeshProbeCommand is the command MeshProbeScript is fed to, for the mesh interface iface (a plain
// interface name, checked by the caller).
func MeshProbeCommand(iface string) string { return "sh -s -- " + iface }

// isMeshProbe reports whether a command and its stdin are exactly the mesh probe.
func isMeshProbe(command string, stdin []byte) bool {
	return meshProbeCmdRe.MatchString(command) && bytes.Equal(stdin, []byte(MeshProbeScript))
}

// readOnlyProbe runs the mesh probe through inner when command and stdin are exactly it.
func readOnlyProbe(ctx context.Context, inner Executor, command string, stdin io.Reader) (string, bool, error) {
	if stdin == nil || !meshProbeCmdRe.MatchString(command) {
		return "", false, nil
	}
	b, err := io.ReadAll(io.LimitReader(stdin, int64(len(MeshProbeScript))+1))
	if err != nil || !isMeshProbe(command, b) {
		return "", false, nil
	}
	out, err := inner.RunWithInput(ctx, command, strings.NewReader(MeshProbeScript))
	return out, true, err
}
