package main

import (
	"testing"
	"time"
)

func TestQuitAndRestartEndpoints(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/quit", "/api/restart"} {
		if c := e.do("POST", p, "", nil).code; c != 401 {
			t.Fatalf("%s without a token: %d", p, c)
		}
		if c := e.do("GET", p, "", tok).code; c != 405 {
			t.Fatalf("%s must be POST only: %d", p, c)
		}
		if c := e.do("POST", p, "", tok).code; c != 501 {
			t.Fatalf("%s with no way to exit: %d", p, c)
		}
	}

	got := make(chan bool, 2)
	e.a.Exit = func(restart bool) { got <- restart }
	if r := e.do("POST", "/api/restart", "", tok); r.code != 200 || r.json()["restarting"] != true {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if r := e.do("POST", "/api/quit", "", tok); r.code != 200 || r.json()["quitting"] != true {
		t.Fatalf("%d %s", r.code, r.body)
	}
	seen := map[bool]bool{}
	for i := 0; i < 2; i++ {
		select {
		case restart := <-got:
			seen[restart] = true
		case <-time.After(2 * time.Second):
			t.Fatal("Exit was not called")
		}
	}
	if !seen[true] || !seen[false] {
		t.Fatalf("expected one restart and one quit, got %v", seen)
	}
}
