package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
)

type fakeRegistrar struct {
	calls []string
	fp    string
	err   error
}

func (f *fakeRegistrar) EnsureSSHKey(_ context.Context, name, pub string) (string, error) {
	f.calls = append(f.calls, name+"|"+pub)
	return f.fp, f.err
}

const testPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest operator@laptop"

func TestPrepareAccessRegistersTheKeyAndAuthorizesItInCloudInit(t *testing.T) {
	reg := &fakeRegistrar{fp: "aa:bb:cc"}
	spec := config.HostSpec{Name: "node-01", Provider: "digitalocean"}
	if err := prepareAccess(context.Background(), reg, &spec, testPub, false, false); err != nil {
		t.Fatal(err)
	}
	if len(reg.calls) != 1 || reg.calls[0] != "node-01-key|"+testPub {
		t.Fatalf("the key must be registered once, under the host's name: %v", reg.calls)
	}
	if len(spec.SSHKeyNames) != 1 || spec.SSHKeyNames[0] != "aa:bb:cc" {
		t.Fatalf("the registered key must be attached to the host: %v", spec.SSHKeyNames)
	}
	if !strings.Contains(spec.UserData, testPub) || !strings.HasPrefix(spec.UserData, "#cloud-config") {
		t.Fatalf("cloud-init must authorize the key too: %q", spec.UserData)
	}
}

func TestPrepareAccessNeverCreatesAHostNobodyCanLogInTo(t *testing.T) {
	// A throwaway key is refused for `host create`: it is discarded when the command exits.
	reg := &fakeRegistrar{fp: "aa"}
	spec := config.HostSpec{Name: "n"}
	err := prepareAccess(context.Background(), reg, &spec, testPub, true, false)
	if err == nil || !strings.Contains(err.Error(), "SSH_PRIVATE_KEY") {
		t.Fatalf("got %v", err)
	}
	if len(reg.calls) != 0 || len(spec.SSHKeyNames) != 0 || spec.UserData != "" {
		t.Fatalf("a refused request must not register or change anything: %v %+v", reg.calls, spec)
	}

	// ...but is fine for reconcile, which uses it within the same run.
	if err := prepareAccess(context.Background(), reg, &spec, testPub, true, true); err != nil {
		t.Fatalf("reconcile may use a generated key: %v", err)
	}

	// A registration failure used to be swallowed, creating a locked-out droplet.
	reg = &fakeRegistrar{err: errors.New("401 unauthorized")}
	spec = config.HostSpec{Name: "n"}
	err = prepareAccess(context.Background(), reg, &spec, testPub, false, false)
	if err == nil || !strings.Contains(err.Error(), "cannot be logged in to") || len(spec.SSHKeyNames) != 0 || spec.UserData != "" {
		t.Fatalf("a failed registration must stop before anything is created and leave the spec alone: %v %+v", err, spec)
	}

	for name, tc := range map[string]struct {
		reg *fakeRegistrar
		pub string
	}{
		"empty public key":    {&fakeRegistrar{fp: "aa"}, "  "},
		"no fingerprint back": {&fakeRegistrar{fp: ""}, testPub},
	} {
		spec = config.HostSpec{Name: "n"}
		if err := prepareAccess(context.Background(), tc.reg, &spec, tc.pub, false, false); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestPrepareAccessKeepsWhatTheCallerAlreadySet(t *testing.T) {
	reg := &fakeRegistrar{fp: "aa:bb"}
	spec := config.HostSpec{Name: "n", SSHKeyNames: []string{"11:22", "aa:bb"}, UserData: "#cloud-config\nruncmd: [true]\n"}
	if err := prepareAccess(context.Background(), reg, &spec, testPub, false, false); err != nil {
		t.Fatal(err)
	}
	if len(spec.SSHKeyNames) != 2 {
		t.Fatalf("the fingerprint must not be added twice: %v", spec.SSHKeyNames)
	}
	if spec.UserData != "#cloud-config\nruncmd: [true]\n" {
		t.Fatalf("caller-provided user data must be kept: %q", spec.UserData)
	}
}

func TestPrepareAccessLeavesOtherProvidersAlone(t *testing.T) {
	h := &HostManager{}
	spec := config.HostSpec{Name: "pve-1", Provider: "proxmox"}
	if err := h.PrepareAccess(context.Background(), &spec, "", true, false); err != nil || len(spec.SSHKeyNames) != 0 || spec.UserData != "" {
		t.Fatalf("Proxmox is not handled here: %v %+v", err, spec)
	}
	if err := h.PrepareAccess(context.Background(), &config.HostSpec{Name: "x", Provider: "digitalocean"}, testPub, false, false); err == nil {
		t.Fatal("DigitalOcean without a client must be an error")
	}
}
