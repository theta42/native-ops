package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/incus"
)

func TestValidateLabels(t *testing.T) {
	ok := []map[string]string{
		nil,
		{},
		{"environment": "production", "app": "platform", "upgrade-window": "us-east-night"},
		{"environment": "development"},
	}
	for _, l := range ok {
		if err := ValidateLabels(l); err != nil {
			t.Errorf("%v: %v", l, err)
		}
	}
	bad := map[string]map[string]string{
		"an environment outside the fixed set": {"environment": "prod"},
		"a typo of one":                        {"environment": "Production"},
		"a key with capitals":                  {"Environment": "production"},
		"a key with a dot":                     {"a.b": "x"},
		"a key starting with a digit":          {"1a": "x"},
		"an empty value":                       {"app": ""},
		"a value with a space":                 {"app": "a b"},
		"a value with a line break":            {"app": "a\nb"},
		"a value that is too long":             {"app": strings.Repeat("a", 64)},
		"a key that is too long":               {strings.Repeat("a", 33): "x"},
	}
	for why, l := range bad {
		if ValidateLabels(l) == nil {
			t.Errorf("%s was accepted: %v", why, l)
		}
	}
	many := map[string]string{}
	for i := 0; i < MaxLabels+1; i++ {
		many["k"+strings.Repeat("a", i)] = "v"
	}
	if ValidateLabels(many) == nil {
		t.Error("more than MaxLabels labels were accepted")
	}
	for _, e := range Environments {
		if err := ValidateLabels(map[string]string{"environment": e}); err != nil {
			t.Errorf("%s must be a valid environment: %v", e, err)
		}
	}
}

func TestASpecCarriesAndChecksItsLabels(t *testing.T) {
	spec := InstanceSpec{Template: "platform", Image: "opsavor-platform:latest", Labels: map[string]string{"environment": "staging", "app": "platform"}}
	if err := spec.Validate("rest-x", DefaultInstancePolicy); err != nil {
		t.Fatal(err)
	}
	if got := spec.TemplateConfig().Labels; got["environment"] != "staging" || got["app"] != "platform" {
		t.Fatalf("the template config must carry the labels: %v", got)
	}
	spec.Labels = map[string]string{"environment": "prod"}
	if err := spec.Validate("rest-x", DefaultInstancePolicy); err == nil || !strings.Contains(err.Error(), "environment") {
		t.Fatalf("an unknown environment must be refused: %v", err)
	}
	var none InstanceSpec
	none.Template, none.Image = "platform", "opsavor-platform:latest"
	if none.TemplateConfig().Labels != nil {
		t.Fatal("a spec without labels must leave an existing instance's labels alone (nil, not empty)")
	}
}

func TestLabelsOfReadsOnlyTheLabelKeys(t *testing.T) {
	got := incus.LabelsOf(map[string]string{
		"user.native-ops.label.environment": "production", "user.native-ops.label.app": "crew",
		"user.native-ops.template": "platform", "limits.cpu": "1",
	})
	if len(got) != 2 || got["environment"] != "production" || got["app"] != "crew" {
		t.Fatalf("%v", got)
	}
	if incus.LabelsOf(map[string]string{"limits.cpu": "1"}) != nil {
		t.Fatal("no labels is nil")
	}
}

func TestReconcileLabelsSetsWhatChangedAndRemovesWhatIsNotWanted(t *testing.T) {
	ex := &scriptExec{}
	m := newMgr(ex, nil)
	cfg := map[string]string{
		"user.native-ops.label.environment": "staging", "user.native-ops.label.old": "x", "user.native-ops.label.same": "1",
		"user.native-ops.template": "platform",
	}
	want := map[string]string{"environment": "production", "same": "1", "new": "y"}
	if err := m.reconcileLabels(context.Background(), "rest-x", cfg, want); err != nil {
		t.Fatal(err)
	}
	if ex.count("incus config set 'rest-x' 'user.native-ops.label.environment=production'") != 1 ||
		ex.count("incus config set 'rest-x' 'user.native-ops.label.new=y'") != 1 {
		t.Fatalf("changed and new labels must be set once: %v", ex.cmds)
	}
	if ex.count("label.same") != 0 {
		t.Fatalf("an unchanged label must not be touched: %v", ex.cmds)
	}
	if ex.count("incus config unset 'rest-x' 'user.native-ops.label.old'") != 1 {
		t.Fatalf("a label that is not wanted must be removed: %v", ex.cmds)
	}
	if ex.count("template") != 0 {
		t.Fatalf("only labels are reconciled, never the other bookkeeping keys: %v", ex.cmds)
	}
}

func labelledConfig(extra string) []rule {
	yaml := strings.Replace(configYAML(oldFP), "  limits.cpu: \"2\"\n", "  limits.cpu: \"2\"\n"+extra, 1)
	return []rule{{match: "incus config show", out: yaml}}
}

func TestSetLabelsAppliesTheChangeLiveAndKeepsTheRest(t *testing.T) {
	ex := &scriptExec{rules: labelledConfig("  user.native-ops.label.environment: staging\n  user.native-ops.label.app: platform\n")}
	err := newMgr(ex, nil).SetLabels(context.Background(), "rest-x", map[string]string{"environment": "production"}, []string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if ex.count("label.environment=production") != 1 || ex.count("unset 'rest-x' 'user.native-ops.label.app'") != 1 {
		t.Fatalf("%v", ex.cmds)
	}
	if ex.count("incus delete") != 0 || ex.count("incus launch") != 0 || ex.count("incus restart") != 0 || ex.count("incus stop") != 0 {
		t.Fatalf("a label change must not restart or replace the instance: %v", ex.cmds)
	}
}

func TestSetLabelsRefusesAnInvalidResultBeforeChangingAnything(t *testing.T) {
	ex := &scriptExec{rules: labelledConfig("  user.native-ops.label.environment: staging\n")}
	err := newMgr(ex, nil).SetLabels(context.Background(), "rest-x", map[string]string{"environment": "prod"}, nil)
	if err == nil {
		t.Fatal("an unknown environment must be refused")
	}
	if ex.count("incus config set") != 0 || ex.count("incus config unset") != 0 {
		t.Fatalf("nothing may be changed when the result is invalid: %v", ex.cmds)
	}
}

func TestAnUpdateAndItsRollbackCarryTheLabels(t *testing.T) {
	rules := append(labelledConfig("  user.native-ops.label.environment: production\n"), baseRules(oldFP)...)
	ex := &scriptExec{rules: rules}
	if err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{Service: "platform"}); err != nil {
		t.Fatal(err)
	}
	launch := ex.index("incus launch")
	if launch < 0 || !strings.Contains(ex.cmds[launch], "--config 'user.native-ops.label.environment=production'") {
		t.Fatalf("the replacement must keep the instance's labels: %v", ex.cmds)
	}
}
