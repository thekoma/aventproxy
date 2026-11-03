package rtsp

import (
	"testing"
	"time"
)

// A monitor whose LAN never works (a stale password, an untested model) used
// to be probed every 5 minutes for as long as a cloud stream ran. Each failure
// in a row now doubles the wait, up to an hour.
func TestLANCooldownDoublesUpToAnHour(t *testing.T) {
	want := []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute,
		time.Hour, time.Hour, time.Hour,
	}
	for i, w := range want {
		if got := lanCooldownFor(i + 1); got != w {
			t.Errorf("lanCooldownFor(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := lanCooldownFor(1000); got != lanMaxCooldown {
		t.Errorf("lanCooldownFor(1000) = %v, want the cap %v", got, lanMaxCooldown)
	}
}

func TestRepeatedLANFailuresLengthenTheCooldown(t *testing.T) {
	server := newLANServer()
	if got := server.startLANCooldown("testdev"); got != lanCooldownPeriod {
		t.Fatalf("first failure: %v, want %v", got, lanCooldownPeriod)
	}
	if got := server.startLANCooldown("testdev"); got != 2*lanCooldownPeriod {
		t.Fatalf("second failure: %v, want %v", got, 2*lanCooldownPeriod)
	}
	server.cooldownMutex.Lock()
	left := time.Until(server.lanCooldowns["testdev"])
	server.cooldownMutex.Unlock()
	if left < 2*lanCooldownPeriod-time.Minute {
		t.Fatalf("cool-down has %v left, want about %v", left, 2*lanCooldownPeriod)
	}
	if got := server.startLANCooldown("otherdev"); got != lanCooldownPeriod {
		t.Fatalf("another camera's first failure: %v, want %v", got, lanCooldownPeriod)
	}
}

func TestWorkingLANResetsTheCooldownStreak(t *testing.T) {
	server := newLANServer()
	for range 3 {
		server.startLANCooldown("testdev")
	}
	server.lanWorked("testdev")
	if server.lanCoolingDown("testdev") {
		t.Fatal("a camera whose LAN works must not be cooling down")
	}
	if got := server.startLANCooldown("testdev"); got != lanCooldownPeriod {
		t.Fatalf("first failure after a success: %v, want %v", got, lanCooldownPeriod)
	}
}

func TestLANSuccessEndsTheStreak(t *testing.T) {
	server := newLANServer()
	server.startLANCooldown("testdev")
	server.startLANCooldown("testdev")
	server.cooldownMutex.Lock()
	delete(server.lanCooldowns, "testdev") // the cool-down ran out
	server.cooldownMutex.Unlock()
	cs, _ := newLANStream(t, server, "hd")
	fakeAttempts(t, cs, nil)

	begin(cs)
	t.Cleanup(cs.stopStream)

	if got := server.startLANCooldown("testdev"); got != lanCooldownPeriod {
		t.Fatalf("first failure after the LAN worked: %v, want %v", got, lanCooldownPeriod)
	}
}
