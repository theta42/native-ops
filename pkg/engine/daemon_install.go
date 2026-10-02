package engine

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/theta42/native-ops/deploy"
)

// DaemonInstall asks a new host's cloud-init to install the native-ops daemon, so the host can be
// driven from CI as soon as it boots, with nobody logging in to set it up. The release is downloaded
// from Releases and checked against SHA256 before it is installed; the daemon starts with the
// shipped systemd unit (deploy/systemd) plus ServeFlags.
//
// The bootstrap admin token is given only as its SHA-256. User-data can be read back from the cloud
// provider's metadata service by anything on the host that reaches it, containers included, so the
// secret itself must never be in it; CI keeps the secret and uses it to create every other token over
// the API (POST /v1/tokens).
type DaemonInstall struct {
	Version              string // release tag, e.g. v1.54.0
	SHA256               string // of native-ops_<Version>_linux_<Arch>.tar.gz (from the release's checksums.txt)
	Arch                 string // amd64 (default) or arm64
	BootstrapTokenSHA256 string // hex SHA-256 of the bootstrap admin token
	ServeFlags           string // extra `native-ops serve` flags, e.g. "--enable-apply --enable-edge-apply"
	ReleaseBase          string // default https://github.com/theta42/native-ops/releases/download
}

const defaultReleaseBase = "https://github.com/theta42/native-ops/releases/download"

var (
	versionRe    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-.][A-Za-z0-9.]+)?$`)
	sha256Re     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	serveFlagsRe = regexp.MustCompile(`^[A-Za-z0-9 _=,.:/@-]*$`)
	releaseURLRe = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(/[A-Za-z0-9._~%/-]*)?$`)
)

// Validate reports why the install cannot be written into a cloud-init. Every value ends up in a shell
// command or a unit file, so each is held to a narrow shape.
func (d *DaemonInstall) Validate() error {
	if !versionRe.MatchString(d.Version) {
		return fmt.Errorf("daemon version %q is not a release tag like v1.54.0", d.Version)
	}
	if !sha256Re.MatchString(d.SHA256) {
		return fmt.Errorf("the daemon release needs its SHA-256 (64 lowercase hex characters, from checksums.txt)")
	}
	switch d.Arch {
	case "", "amd64", "arm64":
	default:
		return fmt.Errorf("daemon arch %q is not amd64 or arm64", d.Arch)
	}
	if !sha256Re.MatchString(d.BootstrapTokenSHA256) {
		return fmt.Errorf("the bootstrap token must be given as its SHA-256 (64 lowercase hex characters)")
	}
	if !serveFlagsRe.MatchString(d.ServeFlags) {
		return fmt.Errorf("serve flags %q may only hold letters, digits, spaces and _=,.:/@-", d.ServeFlags)
	}
	if d.ReleaseBase != "" && !releaseURLRe.MatchString(d.ReleaseBase) {
		return fmt.Errorf("release base %q must be a plain https URL", d.ReleaseBase)
	}
	return nil
}

// runcmd is what cloud-init runs, after Incus is installed, to install and start the daemon.
func (d *DaemonInstall) runcmd() []string {
	arch := d.Arch
	if arch == "" {
		arch = "amd64"
	}
	base := strings.TrimRight(d.ReleaseBase, "/")
	if base == "" {
		base = defaultReleaseBase
	}
	name := fmt.Sprintf("native-ops_%s_linux_%s", d.Version, arch)
	url := fmt.Sprintf("%s/%s/%s.tar.gz", base, d.Version, name)
	return []string{
		"incus profile show default >/dev/null 2>&1 || incus admin init --auto",
		"id native-ops >/dev/null 2>&1 || useradd --system --home-dir /var/lib/native-ops --shell /usr/sbin/nologin native-ops",
		"usermod -aG incus-admin native-ops",
		fmt.Sprintf("curl -fsSL --proto '=https' --tlsv1.2 -o /tmp/native-ops.tgz '%s'", url),
		fmt.Sprintf("echo '%s  /tmp/native-ops.tgz' | sha256sum -c -", d.SHA256),
		"tar -xzf /tmp/native-ops.tgz -C /tmp",
		fmt.Sprintf("install -m 0755 '/tmp/%s' /usr/local/bin/native-ops", name),
		fmt.Sprintf("rm -f /tmp/native-ops.tgz '/tmp/%s'", name),
		"systemctl daemon-reload",
		"systemctl enable --now native-ops-serve",
	}
}

// writeFiles is the unit and its environment file.
func (d *DaemonInstall) writeFiles() []map[string]string {
	unit := deploy.ServeUnit
	if f := strings.TrimSpace(d.ServeFlags); f != "" {
		unit = strings.Replace(unit, "--state-dir /var/lib/native-ops\n", "--state-dir /var/lib/native-ops "+f+"\n", 1)
	}
	return []map[string]string{
		{"path": "/etc/systemd/system/native-ops-serve.service", "permissions": "0644", "content": unit},
		{"path": "/etc/native-ops/serve.env", "permissions": "0600",
			"content": "# Written by cloud-init. Only the hash of the bootstrap token: CI holds the token.\nNATIVE_OPS_BOOTSTRAP_TOKEN_SHA256=" + d.BootstrapTokenSHA256 + "\n"},
	}
}

// cloudInitDaemonSection is appended to the cloud-config: more runcmd entries (runcmd is the last key
// of the base document, so they extend its list) and a write_files key.
func (d *DaemonInstall) cloudInitDaemonSection() (string, error) {
	var sb strings.Builder
	sb.WriteString("\n  # 3. Install and start the native-ops daemon (CI drives the host through it)\n")
	for _, c := range d.runcmd() {
		line, err := yaml.Marshal([]string{c})
		if err != nil {
			return "", err
		}
		sb.WriteString("  " + strings.TrimSpace(string(line)) + "\n")
	}
	wf, err := yaml.Marshal(map[string]any{"write_files": d.writeFiles()})
	if err != nil {
		return "", err
	}
	sb.WriteString("\n")
	sb.Write(wf)
	return sb.String(), nil
}
