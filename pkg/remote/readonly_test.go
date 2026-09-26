package remote

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestReadOnlyAllowsOnlyCommandsThatRead(t *testing.T) {
	allowed := []string{
		"incus list --format json",
		"incus list 'gitea' --format json",
		"incus list 'remote:' --format json",
		"incus info 'gitea'",
		"incus config show 'gitea'",
		"incus config show 'remote:gitea'",
		"incus storage volume show 'default' 'gitea-data'",
		"incus storage volume list 'default' --format json",
		"incus storage volume list 'remote:default' --format json",
		"incus image list --format json",
		"incus image alias list --format json",
		"incus profile list 'remote:' --format json",
		"incus profile show 'base'",
		"incus file pull 'edge/etc/caddy/Caddyfile' -",
		"incus file pull 'gitea/etc/default/gitea' -",
		"incus network get incusbr0 ipv4.address",
		"incus network show incusbr0",
	}
	for _, c := range allowed {
		if !IsReadOnly(c) {
			t.Errorf("should be allowed: %s", c)
		}
	}
	refused := []string{
		"", "incus", "ls", "bash -c 'incus list --format json'",
		"incus delete 'gitea' --force", "incus stop 'gitea'", "incus start 'gitea'", "incus restart 'edge'",
		"incus launch 'img' 'x'", "incus config set 'gitea' 'limits.cpu=2'", "incus config device add 'a' 'b' disk",
		"incus config edit 'gitea'", "incus config unset 'gitea' limits.cpu",
		"incus storage volume create 'default' 'v'", "incus storage volume delete default v",
		"incus storage volume snapshot create 'default' 'v' 's'",
		"incus file push - 'gitea/etc/x'", "incus file pull 'gitea/etc/x' /etc/cron.d/x", "incus file pull 'a' -x", "incus file pull -r 'a' -",
		"incus exec 'edge' -- cat /etc/caddy/Caddyfile", "incus exec 'edge' -- rm -f x",
		"incus profile create x", "incus network set incusbr0 ipv4.nat=true",
		// shell tricks appended to something that on its own reads
		"incus list --format json; incus delete x", "incus list --format json && incus stop x", "incus list --format json || true",
		"incus list --format json | sh", "incus list --format json > /etc/passwd", "incus list --format json >> /tmp/x",
		"incus list --format json &", "incus list $(incus stop x) --format json", "incus list `id` --format json",
		"incus list --format json\nincus delete x", "incus list \"x\" --format json", "incus list x* --format json",
		"incus info gitea; rm -rf /", "incus info gitea;id", "incus info gitea&&id", "incus info gitea||id", "incus info gitea|sh",
		"incus info gitea>x", "incus info gitea<x", "incus info $HOME", "incus info gitea&", "incus info (gitea)", "incus info ~", "incus info #x", "incus config show x\\ y", "incus list '--format json",
		"incus list --format=json", "incus list --format yaml", "incus list -f json",
		"incus list -c n --format json", "incus info", "incus info a b", "incus config show", "incus storage volume show 'default'",
	}
	for _, c := range refused {
		if IsReadOnly(c) {
			t.Errorf("must be refused: %q", c)
		}
	}
}

func TestQuotedContentIsInertAndKeptIntact(t *testing.T) {
	// Single-quoted text may contain shell metacharacters: the shell will not act on them.
	if !IsReadOnly("incus info 'a;b|c$(d)`e`>f'") {
		t.Error("quoted metacharacters are inert")
	}
	w, ok := shellWords(`incus file pull 'a'\''b' -`)
	if !ok || len(w) != 5 || w[3] != "a'b" {
		t.Errorf("the '\\'' idiom should give one word, got %q ok=%v", w, ok)
	}
}

type fakeExec struct{ ran []string }

func (f *fakeExec) Run(_ context.Context, c string) (string, error) {
	f.ran = append(f.ran, c)
	return "ran", nil
}
func (f *fakeExec) RunWithInput(_ context.Context, c string, _ io.Reader) (string, error) {
	f.ran = append(f.ran, c)
	return "ran", nil
}
func (f *fakeExec) WriteFile(_ context.Context, p string, _ []byte, _ os.FileMode) error {
	f.ran = append(f.ran, "write "+p)
	return nil
}
func (f *fakeExec) Close() error { return nil }

func TestTheWrapperNeverForwardsARefusedCommand(t *testing.T) {
	inner := &fakeExec{}
	ex := ReadOnly(inner)
	ctx := context.Background()

	if out, err := ex.Run(ctx, "incus list --format json"); err != nil || out != "ran" {
		t.Fatalf("a read must pass through: %q %v", out, err)
	}
	for _, c := range []string{"incus delete x", "incus list --format json; id"} {
		_, err := ex.Run(ctx, c)
		var ro *ReadOnlyError
		if !errors.As(err, &ro) || !strings.Contains(err.Error(), c) {
			t.Errorf("%q: want a ReadOnlyError naming the command, got %v", c, err)
		}
	}
	if _, err := ex.RunWithInput(ctx, "incus list --format json", strings.NewReader("x")); err == nil {
		t.Error("anything given stdin is refused, even a read")
	}
	if err := ex.WriteFile(ctx, "/etc/x", []byte("x"), 0o644); err == nil {
		t.Error("file writes are refused")
	}
	if len(inner.ran) != 1 || inner.ran[0] != "incus list --format json" {
		t.Fatalf("only the read may reach the real executor, got %v", inner.ran)
	}
}
