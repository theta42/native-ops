package incus

import (
	"reflect"
	"testing"
)

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"plain":       "'plain'",
		"a b":         "'a b'",
		"it's":        `'it'\''s'`,
		"$(rm -rf /)": "'$(rm -rf /)'",
	}
	for in, want := range cases {
		if got := ShQuote(in); got != want {
			t.Errorf("ShQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"rest-acme", "gitea", "a.b_c-1"} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-x", "a b", "a;b", "a$(b)", "a/b", "x'y"} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestParseInstanceState(t *testing.T) {
	y := "config:\n  image.os: Debian\n  limits.cpu: \"2\"\n  security.nesting: \"true\"\n  volatile.base_image: abc123\n  volatile.last_state.power: RUNNING\n" +
		"devices:\n  data:\n    pool: default\n    source: vol-data\n    path: /app/.data\n    type: disk\n  host:\n    source: /srv/x\n    path: /x\n    type: disk\n" +
		"profiles:\n- default\n- base\n"
	st, err := ParseInstanceState("c1", y)
	if err != nil {
		t.Fatal(err)
	}
	if st.BaseImage != "abc123" {
		t.Errorf("BaseImage = %q", st.BaseImage)
	}
	if want := map[string]string{"limits.cpu": "2", "security.nesting": "true"}; !reflect.DeepEqual(st.Config, want) {
		t.Errorf("Config = %v, want %v (no volatile.* / image.*)", st.Config, want)
	}
	if !reflect.DeepEqual(st.Profiles, []string{"default", "base"}) {
		t.Errorf("Profiles = %v", st.Profiles)
	}
	// Only the custom-volume disk counts as data to snapshot; a host bind mount does not.
	if got := st.Volumes(); !reflect.DeepEqual(got, []VolumeRef{{Pool: "default", Name: "vol-data"}}) {
		t.Errorf("Volumes = %v", got)
	}
	if !reflect.DeepEqual(st.DeviceNames(), []string{"data", "host"}) {
		t.Errorf("DeviceNames = %v", st.DeviceNames())
	}
	if _, err := ParseInstanceState("c1", "config: [unclosed"); err == nil {
		t.Error("malformed YAML must be an error")
	}
}
