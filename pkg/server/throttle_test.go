package server

import (
	"testing"
	"time"
)

func TestLoginThrottleBacksOffAndForgetsOnSuccess(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	th := newLoginThrottle()
	th.now = func() time.Time { return now }

	for i := 0; i < throttleFree; i++ {
		if th.Wait("Sam") != 0 {
			t.Fatalf("attempt %d must be allowed", i+1)
		}
		th.Fail("sam")
	}
	if th.Wait("sam") != 0 {
		t.Fatal("the free failures must not lock the account yet")
	}
	th.Fail("sam")
	if w := th.Wait(" SAM "); w != throttleFirst {
		t.Fatalf("first wait = %v, want %v", w, throttleFirst)
	}
	now = now.Add(throttleFirst)
	th.Fail("sam")
	if w := th.Wait("sam"); w != 2*throttleFirst {
		t.Fatalf("the wait must double: %v", w)
	}
	for i := 0; i < 20; i++ {
		th.Fail("sam")
	}
	if w := th.Wait("sam"); w != throttleMax {
		t.Fatalf("the wait must stop at the ceiling: %v", w)
	}
	if th.Wait("someone-else") != 0 {
		t.Fatal("another account must not be affected")
	}
	th.Succeed("sam")
	if th.Wait("sam") != 0 {
		t.Fatal("a success must clear the account")
	}
}
