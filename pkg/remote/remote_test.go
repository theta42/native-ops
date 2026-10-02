package remote

import (
	"context"
	"strings"
	"testing"
)

func TestCommandHeadKeepsOnlyTheFirstWords(t *testing.T) {
	cases := map[string]string{
		"incus list --format json":                        "incus list --format json",
		"incus config set 'web' 'environment.KEY=s3cret'": "incus config set 'web' ...",
		"   incus    info  'web'  ":                       "incus info 'web'",
		"echo " + strings.Repeat("x", 200):                "echo " + strings.Repeat("x", 75) + "...",
	}
	for in, want := range cases {
		if got := CommandHead(in); got != want {
			t.Errorf("CommandHead(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocalErrorsDoNotRepeatTheWholeCommand(t *testing.T) {
	_, err := NewLocalExecutor().Run(context.Background(), "false 'arg1' 'arg2' 'arg3' 's3cret-value'")
	if err == nil {
		t.Fatal("false must fail")
	}
	if strings.Contains(err.Error(), "s3cret-value") {
		t.Fatalf("the error repeats an argument it must not: %v", err)
	}
}

func TestLocalRunWithInputFeedsStdin(t *testing.T) {
	out, err := NewLocalExecutor().RunWithInput(context.Background(), "cat", strings.NewReader("hello"))
	if err != nil || out != "hello" {
		t.Fatalf("got %q, %v", out, err)
	}
}
