package labels

import (
	"strings"
	"testing"
)

func TestEnvironmentIsAClosedSetAndOtherLabelsAreFree(t *testing.T) {
	for _, e := range Environments {
		if err := Validate(map[string]string{Environment: e}); err != nil {
			t.Errorf("%s: %v", e, err)
		}
	}
	for _, bad := range []string{"prod", "Production", "", "production ", "stagin"} {
		if Validate(map[string]string{Environment: bad}) == nil {
			t.Errorf("environment %q must be refused", bad)
		}
	}
	if err := Validate(map[string]string{"app": "platform", "upgrade-window": "us-east-night", "tier": "1.2_x"}); err != nil {
		t.Fatal(err)
	}
	if Validate(map[string]string{"Bad": "x"}) == nil || Validate(map[string]string{"a": "b c"}) == nil || Validate(map[string]string{"a": strings.Repeat("x", 64)}) == nil {
		t.Fatal("a malformed name or value must be refused")
	}
	many := map[string]string{}
	for i := 0; i <= Max; i++ {
		many["k"+strings.Repeat("a", i)] = "v"
	}
	if Validate(many) == nil {
		t.Fatal("more than Max labels must be refused")
	}
	if !ValidKey("environment") || ValidKey("1x") || ValidKey("a.b") {
		t.Fatal("ValidKey")
	}
}
