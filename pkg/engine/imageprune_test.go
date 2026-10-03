package engine

import (
	"context"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

var pruneNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func img(fp string, ageHours int, aliases ...string) HostImage {
	return HostImage{Fingerprint: fp, Created: pruneNow.Add(-time.Duration(ageHours) * time.Hour), Size: 300 << 20, Aliases: aliases}
}

func pruned(p []PrunedImage) []string {
	out := make([]string, 0, len(p))
	for _, x := range p {
		out = append(out, x.Fingerprint)
	}
	sort.Strings(out)
	return out
}

func TestImagesToPruneKeepsWhatRunsAndWhatIsNamedByHand(t *testing.T) {
	images := []HostImage{
		img("orphan-old", 72),
		img("orphan-new", 2),
		img("orphan-running", 72),
		{Fingerprint: "cached-remote", Created: pruneNow.Add(-100 * time.Hour), Cached: true},
		img("base", 300, "app-base"),                    // no :ref, so not a build's
		img("hand", 300, "debian-golden:13"),            // another prefix
		img("mixed", 300, "app-web:v1", "my-favourite"), // one alias is not a build's
	}
	r := ImageRetention{Prefix: "app-", KeepTags: 1, OrphanAge: 24 * time.Hour}
	got := pruned(ImagesToPrune(images, map[string]bool{"orphan-running": true}, r, pruneNow))
	if strings.Join(got, ",") != "orphan-old" {
		t.Fatalf("pruned %v; only the old orphan no instance runs may go", got)
	}
}

func TestImagesToPruneKeepsTheNewestTagsAndLatestPerApp(t *testing.T) {
	images := []HostImage{
		img("web-v5", 1, "app-web:v5", "app-web:latest"),
		img("web-v4", 2, "app-web:v4"),
		img("web-v3", 3, "app-web:v3"),
		img("web-v2", 4, "app-web:v2"),
		img("web-v1", 5, "app-web:v1"),
		img("web-v0", 6, "app-web:v0"), // an instance still runs it
		img("api-main", 9, "app-api:main", "app-api:latest"),
		img("api-v1", 50, "app-api:v1"),
	}
	r := ImageRetention{Prefix: "app-", KeepTags: 3, OrphanAge: 24 * time.Hour}
	got := pruned(ImagesToPrune(images, map[string]bool{"web-v0": true}, r, pruneNow))
	if strings.Join(got, ",") != "web-v1,web-v2" {
		t.Fatalf("pruned %v; want the web tags past the newest 3 that nothing runs", got)
	}
	r.App = "api"
	if got := pruned(ImagesToPrune(images, map[string]bool{"web-v0": true}, r, pruneNow)); len(got) != 0 {
		t.Fatalf("a build of api must only touch api's images, and api has 2 of 3: %v", got)
	}
	r.KeepTags = 0
	r.App = ""
	if got := ImagesToPrune(images, nil, r, pruneNow); len(got) != 0 {
		t.Fatalf("--image-keep 0 keeps every tag: %v", pruned(got))
	}
	r.KeepTags, r.Prefix = 3, ""
	if got := ImagesToPrune(images, nil, r, pruneNow); len(got) != 0 {
		t.Fatalf("without a prefix no image is known to be a build's: %v", pruned(got))
	}
}

func TestABuildDeletesTheImageItDisplacedAtOnce(t *testing.T) {
	images := []HostImage{
		img("new", 0, "app-web:main", "app-web:latest"),
		img("displaced", 0),
		img("other-orphan", 0),
		img("old-orphan", 72),
	}
	r := ImageRetention{Prefix: "app-", KeepTags: 5, OrphanAge: 24 * time.Hour, App: "web", Displaced: "displaced"}
	got := pruned(ImagesToPrune(images, nil, r, pruneNow))
	if strings.Join(got, ",") != "displaced" {
		t.Fatalf("pruned %v; a build removes only what it displaced (orphans are the maintenance job's)", got)
	}
	if got := ImagesToPrune(images, map[string]bool{"displaced": true}, r, pruneNow); len(got) != 0 {
		t.Fatalf("a displaced image an instance still runs stays: %v", pruned(got))
	}
}

// queryExec answers incus queries with canned JSON and records the rest.
type queryExec struct {
	answers map[string]string
	ran     []string
}

func (s *queryExec) Run(_ context.Context, cmd string) (string, error) {
	s.ran = append(s.ran, cmd)
	for prefix, out := range s.answers {
		if strings.HasPrefix(cmd, prefix) {
			return out, nil
		}
	}
	return "", nil
}
func (s *queryExec) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (s *queryExec) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (s *queryExec) Close() error                                                    { return nil }

func TestPruneImagesDeletesOnlyWhenNotADryRun(t *testing.T) {
	ex := &queryExec{answers: map[string]string{
		"incus query '/1.0/images": `[
			{"fingerprint":"aaaa000000000000","created_at":"2026-09-01T00:00:00Z","size":1048576,"cached":false,"aliases":[]},
			{"fingerprint":"bbbb000000000000","created_at":"2026-09-01T00:00:00Z","size":1048576,"cached":false,"aliases":[]},
			{"fingerprint":"cccc000000000000","created_at":"2026-09-01T00:00:00Z","size":1048576,"cached":false,"aliases":[{"name":"app-web:latest"}]}]`,
		"incus query '/1.0/instances": `[{"name":"web","config":{"volatile.base_image":"bbbb000000000000"}}]`,
	}}
	r := ImageRetention{Prefix: "app-", KeepTags: 5, OrphanAge: time.Hour}
	got, err := PruneImages(context.Background(), ex, r, true, nil)
	if err != nil || len(got) != 1 || got[0].Fingerprint != "aaaa000000000000" {
		t.Fatalf("dry run: %v %v", pruned(got), err)
	}
	for _, c := range ex.ran {
		if strings.Contains(c, "image delete") {
			t.Fatalf("a dry run deleted: %s", c)
		}
	}
	if _, err := PruneImages(context.Background(), ex, r, false, nil); err != nil {
		t.Fatal(err)
	}
	if last := ex.ran[len(ex.ran)-1]; last != "incus image delete 'aaaa000000000000'" {
		t.Fatalf("deleted with %q", last)
	}
}
