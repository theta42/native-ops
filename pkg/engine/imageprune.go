package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// Image retention. Every image build publishes <prefix><app>:<ref> and moves that alias (and :latest) to
// the new image, so rebuilding a ref leaves the previous image with no alias, which nothing would ever
// delete; and every release tag keeps an image of its own. Left alone, a host that builds on every
// merge fills its disk. Retention deletes, and only deletes, images that no instance runs:
//
//   - orphans: images with no alias, not a cache of a remote image, older than OrphanAge (an image a
//     build just displaced is deleted at once, see DisplacedBy);
//   - old tags: of the images whose aliases are all <prefix><app>:<ref>, the newest KeepTags per app are
//     kept, and so is any that carries :latest; older ones go.
//
// An instance's own image (volatile.base_image) is never deleted, so a running service, and an update's
// rollback to the image the instance ran, are never affected. A deleted tag can be built again.

// ImageRetention configures PruneImages.
type ImageRetention struct {
	Prefix    string        // the daemon's image prefix; old-tag retention needs one, to know which images are its own
	KeepTags  int           // tagged images kept per app; 0 turns old-tag retention off
	OrphanAge time.Duration // how old an unaliased image must be before it goes
	App       string        // only this app's tagged images (a build's own cleanup); "" for every app
	Displaced string        // a fingerprint a build just moved an alias off: deleted, whatever its age, if it is now an orphan
}

// HostImage is what retention knows of an image.
type HostImage struct {
	Fingerprint string
	Aliases     []string
	Created     time.Time
	Size        int64
	Cached      bool
}

// PrunedImage is an image retention deleted (or, in a dry run, would delete), and why.
type PrunedImage struct {
	Fingerprint string
	Aliases     []string
	Size        int64
	Why         string
}

// ImagesToPrune decides what retention deletes. inUse holds the fingerprints instances run.
func ImagesToPrune(images []HostImage, inUse map[string]bool, r ImageRetention, now time.Time) []PrunedImage {
	var out []PrunedImage
	byApp := map[string][]HostImage{}
	for _, img := range images {
		if inUse[img.Fingerprint] {
			continue
		}
		if len(img.Aliases) == 0 {
			if img.Cached {
				continue
			}
			switch {
			case r.Displaced != "" && img.Fingerprint == r.Displaced:
				out = append(out, PrunedImage{img.Fingerprint, nil, img.Size, "replaced by the build, and no instance runs it"})
			case r.App == "" && r.OrphanAge > 0 && now.Sub(img.Created) >= r.OrphanAge:
				out = append(out, PrunedImage{img.Fingerprint, nil, img.Size, fmt.Sprintf("no alias, no instance runs it, older than %s", r.OrphanAge)})
			}
			continue
		}
		if r.KeepTags <= 0 || r.Prefix == "" {
			continue
		}
		app, ok := ownApp(img.Aliases, r.Prefix)
		if !ok || (r.App != "" && app != r.App) {
			continue
		}
		byApp[app] = append(byApp[app], img)
	}
	apps := make([]string, 0, len(byApp))
	for app := range byApp {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	for _, app := range apps {
		imgs := byApp[app]
		sort.SliceStable(imgs, func(i, j int) bool { return imgs[i].Created.After(imgs[j].Created) })
		kept := 0
		for _, img := range imgs {
			if hasLatest(img.Aliases) || kept < r.KeepTags {
				kept++
				continue
			}
			out = append(out, PrunedImage{img.Fingerprint, img.Aliases, img.Size, fmt.Sprintf("older than the newest %d %s%s images, and no instance runs it", r.KeepTags, r.Prefix, app)})
		}
	}
	return out
}

// ownApp is the app every alias of an image names as <prefix><app>:<ref>; ok is false when any alias
// is something else (an image someone named by hand is not retention's to delete).
func ownApp(aliases []string, prefix string) (string, bool) {
	app := ""
	for _, a := range aliases {
		rest, ok := strings.CutPrefix(a, prefix)
		if !ok {
			return "", false
		}
		name, ref, ok := strings.Cut(rest, ":")
		if !ok || !ValidImageApp(name) || ref == "" || (app != "" && name != app) {
			return "", false
		}
		app = name
	}
	return app, app != ""
}

func hasLatest(aliases []string) bool {
	for _, a := range aliases {
		if strings.HasSuffix(a, ":latest") {
			return true
		}
	}
	return false
}

// HostImages lists the host's images and the fingerprints its instances run.
func HostImages(ctx context.Context, exec remote.Executor) ([]HostImage, map[string]bool, error) {
	out, err := exec.Run(ctx, "incus query '/1.0/images?recursion=1'")
	if err != nil {
		return nil, nil, fmt.Errorf("list images: %w", err)
	}
	var raw []struct {
		Fingerprint string    `json:"fingerprint"`
		CreatedAt   time.Time `json:"created_at"`
		Size        int64     `json:"size"`
		Cached      bool      `json:"cached"`
		Aliases     []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, nil, fmt.Errorf("parse images: %w", err)
	}
	images := make([]HostImage, 0, len(raw))
	for _, r := range raw {
		img := HostImage{Fingerprint: r.Fingerprint, Created: r.CreatedAt, Size: r.Size, Cached: r.Cached}
		for _, a := range r.Aliases {
			img.Aliases = append(img.Aliases, a.Name)
		}
		images = append(images, img)
	}
	out, err = exec.Run(ctx, "incus query '/1.0/instances?recursion=1'")
	if err != nil {
		return nil, nil, fmt.Errorf("list instances: %w", err)
	}
	var insts []struct {
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &insts); err != nil {
		return nil, nil, fmt.Errorf("parse instances: %w", err)
	}
	inUse := map[string]bool{}
	for _, in := range insts {
		if fp := in.Config["volatile.base_image"]; fp != "" {
			inUse[fp] = true
		}
	}
	return images, inUse, nil
}

// PruneImages applies retention on the host. With dryRun it only reports.
func PruneImages(ctx context.Context, exec remote.Executor, r ImageRetention, dryRun bool, logf func(string, ...any)) ([]PrunedImage, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	images, inUse, err := HostImages(ctx, exec)
	if err != nil {
		return nil, err
	}
	victims := ImagesToPrune(images, inUse, r, time.Now())
	var done []PrunedImage
	var freed int64
	for _, v := range victims {
		name := v.Fingerprint[:12]
		if len(v.Aliases) > 0 {
			name += " (" + strings.Join(v.Aliases, ", ") + ")"
		}
		if dryRun {
			logf("would delete image %s, %d MB: %s", name, v.Size>>20, v.Why)
			done = append(done, v)
			freed += v.Size
			continue
		}
		if _, err := exec.Run(ctx, "incus image delete "+incus.ShQuote(v.Fingerprint)); err != nil {
			logf("could not delete image %s: %v", name, err)
			continue
		}
		logf("deleted image %s, %d MB: %s", name, v.Size>>20, v.Why)
		done = append(done, v)
		freed += v.Size
	}
	verb := "freed"
	if dryRun {
		verb = "would free"
	}
	logf("image retention: %d of %d images, %s %d MB", len(done), len(images), verb, freed>>20)
	return done, nil
}
