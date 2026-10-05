// Package labels is the vocabulary of what a deployed thing is for: the closed set of environments and the
// shape of a label. It has no dependencies so the manifest loader (pkg/config), the engine and the API can
// all validate the same way.
package labels

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Environments is the closed set of values the `environment` label may take. It is fixed in native-ops, not
// configured per host, so every deployment means the same thing by it and a token scoped to
// `environment=production` cannot be defeated by a spelling. Adding a value is a reviewed change here.
var Environments = []string{"production", "staging", "testing", "development", "demo"}

// Environment is the label that names what an instance is for.
const Environment = "environment"

// Max is the most labels one instance may carry.
const Max = 16

var (
	keyRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	valueRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

// ValidKey reports whether key is a usable label name.
func ValidKey(key string) bool { return keyRe.MatchString(key) }

// Validate reports why labels cannot be accepted: at most Max, a plain key and a plain value each, and
// `environment` one of Environments. Any other label is free-form.
func Validate(labels map[string]string) error {
	if len(labels) > Max {
		return fmt.Errorf("at most %d labels", Max)
	}
	for k, v := range labels {
		if !keyRe.MatchString(k) {
			return fmt.Errorf("label name %q is not allowed (a lowercase letter, then lowercase letters, digits or dashes, at most 32 characters)", k)
		}
		if !valueRe.MatchString(v) {
			return fmt.Errorf("the value of label %s is not allowed (1-63 letters, digits, dots, dashes or underscores)", k)
		}
		if k == Environment && !slices.Contains(Environments, v) {
			return fmt.Errorf("environment %q is not one of %s", v, strings.Join(Environments, ", "))
		}
	}
	return nil
}
