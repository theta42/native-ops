package incus

import (
	"fmt"
	"sort"
	"strings"
)

// ImageKey is the instance config key that records the image reference an
// instance was deployed from, as written in the manifest. It lets a later run
// tell "same reference, nothing to do" from "reference changed, replace", which
// fingerprints alone cannot for OCI images.
const ImageKey = "user.native-ops.image"

// TemplateKey marks an instance as launched from a named template, so that
// re-running `instance launch` can resume its own half-finished launch but
// never adopts an unrelated instance that happens to share the name.
const TemplateKey = "user.native-ops.template"

// ParseEnv parses an environment file: KEY=VALUE lines, blank lines and #
// comments ignored, one pair of surrounding quotes stripped from values.
func ParseEnv(text string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			env[k] = v
		}
	}
	return env
}

// RenderEnv renders an environment file with keys in sorted order, so the same
// input always produces byte-identical output.
func RenderEnv(service string, env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Managed by native-ops for %s\n", service)
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s=%s\n", k, env[k])
	}
	return sb.String()
}

// MergeEnv overlays the declared values onto the live ones. Keys that are not
// declared are preserved (they may be runtime secrets pushed by another
// system), so a declared key converges and an undeclared one is never lost.
// changed reports whether the result differs from live.
func MergeEnv(live, declared map[string]string) (merged map[string]string, changed bool) {
	merged = make(map[string]string, len(live)+len(declared))
	for k, v := range live {
		merged[k] = v
	}
	for k, v := range declared {
		if cur, ok := live[k]; !ok || cur != v {
			changed = true
		}
		merged[k] = v
	}
	return merged, changed
}

// SetEnvKeys returns an environment file with the given keys set: a line for a key that is already there
// is replaced (and any later duplicate dropped), keys that are not there are appended in sorted order, and
// every other line -- comments, keys not mentioned -- is kept as it was.
func SetEnvKeys(text string, set map[string]string) string {
	if len(set) == 0 {
		return text
	}
	done := map[string]bool{}
	var out []string
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	for _, line := range lines {
		k, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		k = strings.TrimSpace(k)
		if v, want := set[k]; ok && want && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			if done[k] {
				continue
			}
			out = append(out, k+"="+v)
			done[k] = true
			continue
		}
		out = append(out, line)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		if !done[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return strings.Join(out, "\n") + "\n"
}
