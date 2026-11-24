//go:build unix

package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as the fake bridge and, for the tests that send a
// real signal, as the supervisor process. The first argument picks the role.
const (
	fakeBridgeArg = "fake-bridge"
	supervisorArg = "supervisor-helper"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 {
		switch os.Args[1] {
		case fakeBridgeArg:
			os.Exit(fakeBridge(os.Args[2], os.Args[3:]))
		case supervisorArg:
			os.Exit(supervisorHelper(os.Args[2], os.Args[3]))
		}
	}
	os.Exit(m.Run())
}

// fakeBridge stands in for `avent-webrtc-bridge addon`. The mode file in dir,
// read at every start so that a test can change it while the supervisor runs,
// picks what it does. Every start appends "<unix ns> <pid> <args>" to
// starts.log.
func fakeBridge(dir string, args []string) int {
	// Let go of the go test output pipe, which a bridge left behind by a
	// failed test would otherwise hold open.
	os.Stdout.Close()
	os.Stderr.Close()
	mode, _ := os.ReadFile(filepath.Join(dir, "mode"))
	// Armed before the start is announced: a test stops the supervisor as soon
	// as it sees the start, and a TERM that beat this would end the bridge.
	if string(mode) == "ignoreterm" {
		signal.Ignore(syscall.SIGTERM)
	}
	f, err := os.OpenFile(filepath.Join(dir, "starts.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 2
	}
	_, werr := fmt.Fprintf(f, "%d %d %s\n", time.Now().UnixNano(), os.Getpid(), strings.Join(args, " "))
	if cerr := f.Close(); werr != nil || cerr != nil {
		return 2
	}
	switch string(mode) {
	case "exit0":
		return 0
	case "exit1":
		return 1
	case "exit143":
		return 143
	}
	// "run" and "ignoreterm": the default SIGTERM disposition ends "run".
	time.Sleep(5 * time.Minute)
	return 0
}

// supervisorHelper runs the supervisor as the command does, signal handling
// included, on the harness laid out under root.
func supervisorHelper(root, stopGrace string) int {
	s := fastSupervisor(root, os.Stdout)
	grace, err := time.ParseDuration(stopGrace)
	if err != nil {
		fmt.Println(err)
		return 2
	}
	s.StopGrace = grace
	if err := s.runUntilSignalled(); err != nil {
		fmt.Println(err)
		return 1
	}
	return 0
}

const pollInterval = 100 * time.Millisecond

// fastSupervisor is the add-on's supervisor with the fake bridge and timings
// shrunk from seconds to tenths.
func fastSupervisor(root string, out io.Writer) *Supervisor {
	return &Supervisor{
		ConfigDir:       filepath.Join(root, "config"),
		AddonConfig:     filepath.Join(root, "options.json"),
		WaitForConfig:   true,
		Bridge:          []string{os.Args[0], fakeBridgeArg, root},
		PollInterval:    pollInterval,
		WaitInterval:    pollInterval,
		RestartDelayMin: 100 * time.Millisecond,
		RestartDelayMax: 400 * time.Millisecond,
		StableRun:       time.Minute,
		StopGrace:       2 * time.Second,
		Out:             out,
	}
}

// quiet is how long a test watches for something that must not happen: five
// config checks.
const quiet = 5 * pollInterval

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	t       *testing.T
	root    string // the fake bridge's mode and starts.log, and the dirs below
	options string
	out     *syncBuffer
	sup     *Supervisor

	// In-process runs.
	cancel context.CancelFunc
	done   chan struct{} // closed when Run returns
	err    error         // what Run returned, once done is closed

	// Supervisor process runs.
	proc     *exec.Cmd
	procDone chan struct{} // closed when the process has exited
	logPath  string
}

func newHarness(t *testing.T) *harness {
	t.Parallel()
	root := t.TempDir()
	h := &harness{t: t, root: root, out: &syncBuffer{}}
	h.sup = fastSupervisor(root, h.out)
	h.options = h.sup.AddonConfig
	if err := os.Mkdir(h.sup.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.writeOptions(`{"cameras": []}`)
	h.setMode("run")
	return h
}

func (h *harness) writeOptions(body string) {
	h.t.Helper()
	if err := os.WriteFile(h.options, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// setMode swaps the mode file in one step, so a bridge never reads half of it.
func (h *harness) setMode(mode string) {
	h.t.Helper()
	tmp := filepath.Join(h.root, "mode.tmp")
	if err := os.WriteFile(tmp, []byte(mode), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(h.root, "mode")); err != nil {
		h.t.Fatal(err)
	}
}

// writeConfig writes an integration config the way the integration does, in
// one rename, so that a check never reads it half written. A non-zero mtime is
// set on it.
func (h *harness) writeConfig(name, body string, mtime time.Time) string {
	h.t.Helper()
	tmp := filepath.Join(h.root, name+".tmp")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(tmp, mtime, mtime); err != nil {
			h.t.Fatal(err)
		}
	}
	path := filepath.Join(h.sup.ConfigDir, name)
	if err := os.Rename(tmp, path); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func (h *harness) remove(path string) {
	h.t.Helper()
	if err := os.Remove(path); err != nil {
		h.t.Fatal(err)
	}
}

type start struct {
	at   time.Time
	pid  int
	args string
}

func (h *harness) starts() []start {
	data, err := os.ReadFile(filepath.Join(h.root, "starts.log"))
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	var out []start
	for _, line := range lines[:len(lines)-1] { // the last one is "" or still being written
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 {
			continue
		}
		ns, _ := strconv.ParseInt(parts[0], 10, 64)
		pid, _ := strconv.Atoi(parts[1])
		out = append(out, start{time.Unix(0, ns), pid, parts[2]})
	}
	return out
}

func (h *harness) log() string {
	if h.logPath == "" {
		return h.out.String()
	}
	data, _ := os.ReadFile(h.logPath)
	return string(data)
}

// waitFor polls cond until it holds, failing the test after a deadline that
// leaves room for a loaded machine.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s\nstarts: %v\nlog:\n%s", what, h.starts(), h.log())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) waitForStarts(n int) []start {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%d bridge starts", n), func() bool { return len(h.starts()) >= n })
	return h.starts()
}

func (h *harness) waitForLog(text string) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%q in the log", text), func() bool { return strings.Contains(h.log(), text) })
}

// start runs the supervisor in this process; cancelling its context stands in
// for SIGTERM.
func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		h.err = h.sup.Run(ctx)
		close(h.done)
	}()
	h.t.Cleanup(func() { _ = h.stop() }) // the tests check what Run returned
}

func (h *harness) running() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

func (h *harness) assertRunning() {
	h.t.Helper()
	if !h.running() {
		h.t.Fatalf("Run returned %v\nlog:\n%s", h.err, h.log())
	}
}

// stop ends Run and returns its result.
func (h *harness) stop() error {
	h.cancel()
	select {
	case <-h.done:
		return h.err
	case <-time.After(h.sup.StopGrace + 15*time.Second):
		for _, s := range h.starts() {
			_ = syscall.Kill(s.pid, syscall.SIGKILL)
		}
		h.t.Fatalf("Run did not return after its context ended\nlog:\n%s", h.log())
		return nil
	}
}

// startProcess runs the supervisor as a process of its own, to send it real
// signals.
func (h *harness) startProcess(stopGrace time.Duration) {
	h.t.Helper()
	h.logPath = filepath.Join(h.root, "supervisor.log")
	logFile, err := os.Create(h.logPath)
	if err != nil {
		h.t.Fatal(err)
	}
	h.proc = exec.Command(os.Args[0], supervisorArg, h.root, stopGrace.String())
	h.proc.Stdout, h.proc.Stderr = logFile, logFile
	// A group of its own, so that cleanup can take the bridge down with it.
	h.proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := h.proc.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.procDone = make(chan struct{})
	go func() {
		_ = h.proc.Wait() // the test reads h.proc.ProcessState
		close(h.procDone)
	}()
	h.t.Cleanup(func() {
		_ = syscall.Kill(-h.proc.Process.Pid, syscall.SIGKILL)
		<-h.procDone
		logFile.Close()
	})
}

func (h *harness) signal(sig syscall.Signal) {
	h.t.Helper()
	if err := h.proc.Process.Signal(sig); err != nil {
		h.t.Fatal(err)
	}
}

// waitForExit returns the supervisor process's exit code.
func (h *harness) waitForExit() int {
	h.t.Helper()
	select {
	case <-h.procDone:
		return h.proc.ProcessState.ExitCode()
	case <-time.After(15 * time.Second):
		h.t.Fatalf("the supervisor did not exit\nlog:\n%s", h.log())
		return -1
	}
}

func pidGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestWaitsForIntegrationConfigEvenThoughOptionsJSONExists(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.waitForLog("Waiting for bridge config from HA integration...")
	time.Sleep(quiet)
	if got := h.starts(); len(got) != 0 {
		t.Fatalf("the bridge started on options without credentials: %v", got)
	}

	path := h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
	starts := h.waitForStarts(1)
	if want := "addon --config " + path + " --bind localhost"; starts[0].args != want {
		t.Errorf("args = %q, want %q", starts[0].args, want)
	}
	log := h.log()
	if n := strings.Count(log, "Waiting for bridge config"); n != 1 {
		t.Errorf("the wait was logged %d times, want once:\n%s", n, log)
	}
	if !strings.Contains(log, "Config found!") {
		t.Errorf("no Config found! in the log:\n%s", log)
	}
}

func TestAddonOptionsWithCredentialsAreUsedWhenNoIntegrationConfig(t *testing.T) {
	h := newHarness(t)
	h.writeOptions(`{"signing_key": "k", "sid": "s"}`)
	h.start()
	starts := h.waitForStarts(1)
	if !strings.Contains(starts[0].args, "--config "+h.options+" ") {
		t.Errorf("args = %q, want the add-on options", starts[0].args)
	}
}

func TestAddonOptionsWithoutCredentialsAreNoConfig(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"cameras": []}`,
		`{"signing_key": "k"}`,
		`{"signing_key": "k", "sid": ""}`,
		`{"signing_key": "k", "sid": null}`,
		`not json`,
	} {
		root := t.TempDir()
		s := fastSupervisor(root, io.Discard)
		if err := os.WriteFile(s.AddonConfig, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := s.resolveConfig(); got != "" {
			t.Errorf("%s: resolved %q, want no config", body, got)
		}
	}
}

func TestFailsFastWithoutConfigWhenNotAskedToWait(t *testing.T) {
	h := newHarness(t)
	h.sup.WaitForConfig = false
	h.start()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept waiting")
	}
	if !errors.Is(h.err, ErrNoConfig) || h.err.Error() != "no bridge config found" {
		t.Errorf("Run returned %v, want %v", h.err, ErrNoConfig)
	}
	if got := h.starts(); len(got) != 0 {
		t.Errorf("starts = %v", got)
	}
}

// Through the command, as the image runs it: the error reaches main, which
// exits 1.
func TestCommandFailsFastWithoutConfig(t *testing.T) {
	t.Setenv("WAIT_FOR_CONFIG", "")
	dir := t.TempDir()
	cmd := NewSuperviseCmd()
	var stderr bytes.Buffer
	cmd.SetOut(io.Discard)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--config-dir", dir, "--addon-config", filepath.Join(dir, "options.json")})
	if err := cmd.Execute(); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("Execute returned %v, want %v", err, ErrNoConfig)
	}
	if !strings.Contains(stderr.String(), "no bridge config found") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("a missing config is not a usage mistake:\n%s", stderr.String())
	}
}

func TestWaitsAgainAfterTheFirstStartEvenWhenNotAskedToWait(t *testing.T) {
	h := newHarness(t)
	h.sup.WaitForConfig = false
	path := h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
	h.start()
	starts := h.waitForStarts(1)

	h.remove(path)
	if err := syscall.Kill(starts[0].pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitForLog("Waiting for bridge config from HA integration...")
	h.assertRunning()
}

func TestRestartsTheBridgeOnAnyExitStatus(t *testing.T) {
	t.Parallel()
	for mode, status := range map[string]int{"exit1": 1, "exit0": 0, "exit143": 143} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
			h.setMode(mode)
			h.start()
			h.waitForStarts(3)
			h.assertRunning()
			if want := fmt.Sprintf("Bridge exited with status %d, restarting in", status); !strings.Contains(h.log(), want) {
				t.Errorf("no %q in the log:\n%s", want, h.log())
			}
		})
	}
}

var restartDelay = regexp.MustCompile(`restarting in (\S+)\.\.\.`)

func loggedDelays(log string) []string {
	var delays []string
	for _, m := range restartDelay.FindAllStringSubmatch(log, -1) {
		delays = append(delays, m[1])
	}
	return delays
}

func TestRestartDelayGrowsForACrashLoop(t *testing.T) {
	h := newHarness(t)
	h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
	h.setMode("exit1")
	h.start()
	starts := h.waitForStarts(5)

	// The delay doubles up to the maximum: 0.1, 0.2, 0.4, then 0.4 again.
	want := []string{"0.1s", "0.2s", "0.4s", "0.4s"}
	if got := loggedDelays(h.log()); len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Errorf("logged delays %v, want them to start with %v", got, want)
	}
	// Each restart waits at least its delay; how much longer depends on the
	// machine's load, so only the lower bound is checked.
	for i, d := range []time.Duration{100, 200, 400, 400} {
		if gap := starts[i+1].at.Sub(starts[i].at); gap < d*time.Millisecond {
			t.Errorf("restart %d came %s after the previous start, want at least %dms", i+1, gap, d)
		}
	}
}

func TestRestartDelayResetsAfterAStableRun(t *testing.T) {
	h := newHarness(t)
	h.sup.StableRun = time.Nanosecond // every run counts as stable
	h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
	h.setMode("exit1")
	h.start()
	h.waitForStarts(4)
	for _, d := range loggedDelays(h.log()) {
		if d != "0.1s" {
			t.Fatalf("logged delays %v, want each to be the minimum", loggedDelays(h.log()))
		}
	}
}

func TestKeepsRetryingABridgeThatCannotStart(t *testing.T) {
	h := newHarness(t)
	h.sup.Bridge = []string{filepath.Join(h.root, "missing-binary")}
	h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
	h.start()
	h.waitFor("two failed starts", func() bool { return strings.Count(h.log(), "Bridge could not start: ") >= 2 })
	h.assertRunning()
}

func TestRestartsWhenTheConfigChanges(t *testing.T) {
	h := newHarness(t)
	path := h.writeConfig("philips_avent_bridge_abc.json", `{"a": 1}`, time.Time{})
	h.start()
	first := h.waitForStarts(1)[0]

	h.writeConfig("philips_avent_bridge_abc.json", `{"a": 2}`, time.Time{})
	second := h.waitForStarts(2)[1]
	if !pidGone(first.pid) {
		t.Errorf("the first bridge (pid %d) is still running", first.pid)
	}
	if !strings.Contains(second.args, path) {
		t.Errorf("args = %q", second.args)
	}
	if !strings.Contains(h.log(), "Config changed, restarting bridge...") {
		t.Errorf("log:\n%s", h.log())
	}
	h.assertRunning()
}

// The integration rewrites its file with the same bytes when nothing changed
// (issue #73): a new mtime alone must not restart the stream.
func TestARewriteWithTheSameContentDoesNotRestart(t *testing.T) {
	h := newHarness(t)
	h.writeConfig("philips_avent_bridge_abc.json", `{"a": 1}`, time.Now().Add(-time.Hour))
	h.start()
	h.waitForStarts(1)

	h.writeConfig("philips_avent_bridge_abc.json", `{"a": 1}`, time.Time{})
	time.Sleep(quiet)
	if got := h.starts(); len(got) != 1 {
		t.Fatalf("starts = %v", got)
	}
	// A real change is still seen, which shows the checks were running.
	h.writeConfig("philips_avent_bridge_abc.json", `{"a": 2}`, time.Time{})
	h.waitForStarts(2)
}

func TestASecondValidConfigDoesNotTakeOver(t *testing.T) {
	// Two config entries: both files are valid. Following whichever was written
	// last would restart the stream on every rewrite of the other one.
	h := newHarness(t)
	now := time.Now()
	old := h.writeConfig("philips_avent_bridge_old.json", `{"a": 1}`, now.Add(-100*time.Second))
	h.start()
	h.waitForStarts(1)

	h.writeConfig("philips_avent_bridge_new.json", "{}", now.Add(-50*time.Second))
	time.Sleep(quiet)
	if got := h.starts(); len(got) != 1 {
		t.Fatalf("starts = %v", got)
	}
	h.assertRunning()

	// A change to the current file restarts the bridge on that file, and the
	// check that saw it did not move to the other file either.
	h.writeConfig("philips_avent_bridge_old.json", `{"a": 2}`, time.Time{})
	starts := h.waitForStarts(2)
	if !strings.Contains(starts[1].args, old) {
		t.Errorf("args = %q, want %s", starts[1].args, old)
	}
	if strings.Contains(h.log(), "Bridge config is now") {
		t.Errorf("log:\n%s", h.log())
	}
}

func TestSwitchesWhenTheCurrentConfigIsReplaced(t *testing.T) {
	// A re-added integration: the old entry's file goes, the new one appears,
	// and the running bridge moves to it.
	h := newHarness(t)
	now := time.Now()
	old := h.writeConfig("philips_avent_bridge_old.json", "{}", now.Add(-100*time.Second))
	h.start()
	first := h.waitForStarts(1)[0]

	newPath := h.writeConfig("philips_avent_bridge_new.json", "{}", now)
	h.remove(old)
	second := h.waitForStarts(2)[1]
	if !strings.Contains(second.args, newPath) {
		t.Errorf("args = %q, want %s", second.args, newPath)
	}
	if !pidGone(first.pid) {
		t.Errorf("the first bridge (pid %d) is still running", first.pid)
	}
	if want := "Bridge config is now " + newPath + ", restarting bridge..."; !strings.Contains(h.log(), want) {
		t.Errorf("no %q in the log:\n%s", want, h.log())
	}
}

func TestIntegrationConfigTakesOverFromAddonOptions(t *testing.T) {
	h := newHarness(t)
	h.writeOptions(`{"signing_key": "k", "sid": "s"}`)
	h.start()
	h.waitForStarts(1)

	path := h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
	if second := h.waitForStarts(2)[1]; !strings.Contains(second.args, path) {
		t.Errorf("args = %q, want %s", second.args, path)
	}
}

func TestSwitchesWhenTheCurrentConfigIsRemovedAndTheBridgeExits(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	old := h.writeConfig("philips_avent_bridge_old.json", "{}", now.Add(-100*time.Second))
	h.start()
	h.waitForStarts(1)

	h.setMode("exit1")
	h.remove(old)
	newPath := h.writeConfig("philips_avent_bridge_new.json", "{}", now)
	if second := h.waitForStarts(2)[1]; !strings.Contains(second.args, newPath) {
		t.Errorf("args = %q, want %s", second.args, newPath)
	}
}

func TestWaitsAgainWhenTheConfigVanishesAndTheBridgeDies(t *testing.T) {
	h := newHarness(t)
	path := h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
	h.start()
	first := h.waitForStarts(1)[0]

	h.setMode("exit1")
	h.remove(path)
	if err := syscall.Kill(first.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitForLog("Waiting for bridge config from HA integration...")
	if !strings.Contains(h.log(), "Bridge exited with status 137") {
		t.Errorf("a SIGKILL should read as status 137:\n%s", h.log())
	}
	h.assertRunning()
	if got := h.starts(); len(got) != 1 {
		t.Fatalf("the bridge started without a config: %v", got)
	}

	h.writeConfig("philips_avent_bridge_def.json", "{}", time.Time{})
	h.waitForStarts(2)
}

func TestCancelStopsTheBridge(t *testing.T) {
	h := newHarness(t)
	h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
	h.start()
	first := h.waitForStarts(1)[0]
	if err := h.stop(); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if !pidGone(first.pid) {
		t.Errorf("the bridge (pid %d) outlived the supervisor", first.pid)
	}
}

func TestSignalStopsQuicklyAndTakesTheBridgeDown(t *testing.T) {
	t.Parallel()
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			h := newHarness(t)
			h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
			// A grace period no run gets near: a stop that took it would mean
			// the bridge was killed rather than asked to stop.
			const grace = 30 * time.Second
			h.startProcess(grace)
			pid := h.waitForStarts(1)[0].pid

			began := time.Now()
			h.signal(sig)
			if code := h.waitForExit(); code != 0 {
				t.Errorf("exit code %d, want 0\nlog:\n%s", code, h.log())
			}
			if took := time.Since(began); took > grace/3 {
				t.Errorf("stopping took %s", took)
			}
			if !pidGone(pid) {
				t.Errorf("the bridge (pid %d) outlived the supervisor", pid)
			}
			if log := h.log(); !strings.Contains(log, "Stopping bridge...") || strings.Contains(log, "did not stop in time") {
				t.Errorf("log:\n%s", log)
			}
		})
	}
}

func TestSIGTERMKillsABridgeThatIgnoresIt(t *testing.T) {
	h := newHarness(t)
	h.writeConfig("philips_avent_bridge_abc.json", "{}", time.Time{})
	h.setMode("ignoreterm")
	h.startProcess(300 * time.Millisecond)
	pid := h.waitForStarts(1)[0].pid

	h.signal(syscall.SIGTERM)
	if code := h.waitForExit(); code != 0 {
		t.Errorf("exit code %d, want 0\nlog:\n%s", code, h.log())
	}
	if !pidGone(pid) {
		t.Errorf("the bridge (pid %d) outlived the supervisor", pid)
	}
	if !strings.Contains(h.log(), "Bridge did not stop in time, killing it") {
		t.Errorf("log:\n%s", h.log())
	}
}

func TestSIGTERMWhileWaitingForAConfigExitsCleanly(t *testing.T) {
	h := newHarness(t)
	h.startProcess(2 * time.Second)
	h.waitForLog("Waiting for bridge config from HA integration...")

	h.signal(syscall.SIGTERM)
	if code := h.waitForExit(); code != 0 {
		t.Errorf("exit code %d, want 0\nlog:\n%s", code, h.log())
	}
}

func TestPicksTheNewestConfigAndWarnsAboutTheOthers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var out bytes.Buffer
	s := fastSupervisor(root, &out)
	if err := os.Mkdir(s.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var paths []string
	for i, name := range []string{"philips_avent_bridge_b.json", "philips_avent_bridge_c.json", "philips_avent_bridge_a.json"} {
		path := filepath.Join(s.ConfigDir, name)
		mtime := now.Add(time.Duration(i-10) * time.Minute)
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	// Neither the legacy file nor something that only looks like a config wins.
	for _, name := range []string{"philips_avent_bridge.json", "philips_avent_bridge_x.json.tmp"} {
		if err := os.WriteFile(filepath.Join(s.ConfigDir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(s.ConfigDir, "philips_avent_bridge_dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	if got := s.resolveConfig(); got != paths[2] {
		t.Errorf("resolved %s, want the newest, %s", got, paths[2])
	}
	s.warnOnExtraConfigs()
	want := fmt.Sprintf("WARNING: 3 bridge config files found in %s, using the newest one:\n  %s\n  %s\n  %s\n",
		s.ConfigDir, paths[2], paths[1], paths[0])
	if !strings.HasPrefix(out.String(), want) {
		t.Errorf("warning:\n%s\nwant it to start with:\n%s", out.String(), want)
	}
}

func TestLegacyConfig(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := fastSupervisor(root, io.Discard)
	if err := os.Mkdir(s.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.AddonConfig, []byte(`{"signing_key": "k", "sid": "s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(s.ConfigDir, "philips_avent_bridge.json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveConfig(); got != legacy {
		t.Errorf("resolved %s, want the legacy file over the add-on options", got)
	}
	entry := filepath.Join(s.ConfigDir, "philips_avent_bridge_abc.json")
	if err := os.WriteFile(entry, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(entry, old, old); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveConfig(); got != entry {
		t.Errorf("resolved %s, want the per-entry file over the newer legacy one", got)
	}
}

func TestBridgeArgs(t *testing.T) {
	t.Parallel()
	base := []string{"addon", "--config", "/config/c.json", "--bind", "localhost"}
	for _, tc := range []struct {
		name    string
		options string // "" leaves options.json out
		want    []string
	}{
		{"no options file", "", base},
		{"options without knobs", `{"cameras": []}`, base},
		{"stable card", `{"cameras": [], "rtsp_bind_address": "localhost", "cloud_transport": "webrtc", "talkback_mode": "on_demand"}`,
			append(slices.Clone(base), "--cloud-transport", "webrtc", "--talkback-mode", "on_demand")},
		{"dev card", `{"speaker_off": true, "webrtc_audio": "recvonly", "cloud_transport": "imm", "talkback_mode": "session"}`,
			append(slices.Clone(base), "--speaker-off=true", "--webrtc-audio", "recvonly", "--cloud-transport", "imm", "--talkback-mode", "session")},
		{"a false still passes", `{"speaker_off": false}`, append(slices.Clone(base), "--speaker-off=false")},
		{"null and empty are left out", `{"speaker_off": null, "webrtc_audio": ""}`, base},
		{"broken options", `{"speaker_off": tru`, base},
	} {
		root := t.TempDir()
		s := fastSupervisor(root, io.Discard)
		if tc.options != "" {
			if err := os.WriteFile(s.AddonConfig, []byte(tc.options), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got := s.bridgeArgs("/config/c.json", "localhost"); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRTSPBind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, bind, options, want string
	}{
		{"default", "", `{}`, "localhost"},
		{"add-on option", "", `{"rtsp_bind_address": "0.0.0.0"}`, "0.0.0.0"},
		{"blank add-on option", "", `{"rtsp_bind_address": ""}`, "localhost"},
		{"environment over the option", "192.168.1.5", `{"rtsp_bind_address": "0.0.0.0"}`, "192.168.1.5"},
	} {
		root := t.TempDir()
		s := fastSupervisor(root, io.Discard)
		s.Bind = tc.bind
		if err := os.WriteFile(s.AddonConfig, []byte(tc.options), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := s.rtspBind(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBannerWarnsWhenRTSPLeavesLoopback(t *testing.T) {
	t.Parallel()
	for bind, warns := range map[string]bool{
		"localhost": false, "127.0.0.1": false, "::1": false, "127.0.0.1,::1": false,
		"0.0.0.0": true, "192.168.1.5": true,
	} {
		var out bytes.Buffer
		s := &Supervisor{Out: &out}
		s.logBanner(bind)
		if !strings.Contains(out.String(), "RTSP bind: "+bind+"\n") {
			t.Errorf("%s: banner:\n%s", bind, out.String())
		}
		if got := strings.Contains(out.String(), "has no password"); got != warns {
			t.Errorf("%s: warning shown %t, want %t", bind, got, warns)
		}
	}
}

func TestCommandFlagDefaults(t *testing.T) {
	for _, key := range []string{"CONFIG_DIR", "ADDON_CONFIG", "WAIT_FOR_CONFIG", "RTSP_BIND_ADDRESS"} {
		t.Setenv(key, "")
	}
	assertFlags(t, map[string]string{
		"config-dir": "/config", "addon-config": "/data/options.json", "wait-for-config": "false", "bind": "",
		"poll-interval": "10s", "wait-interval": "5s", "restart-delay-min": "5s", "restart-delay-max": "5m0s",
		"stable-run": "1m0s", "stop-grace": "8s",
	})

	// The add-on's config.yaml sets WAIT_FOR_CONFIG; docker-compose.yml sets
	// RTSP_BIND_ADDRESS too.
	t.Setenv("CONFIG_DIR", "/srv/config")
	t.Setenv("ADDON_CONFIG", "/srv/options.json")
	t.Setenv("WAIT_FOR_CONFIG", "true")
	t.Setenv("RTSP_BIND_ADDRESS", "0.0.0.0")
	assertFlags(t, map[string]string{
		"config-dir": "/srv/config", "addon-config": "/srv/options.json", "wait-for-config": "true", "bind": "0.0.0.0",
	})
}

func assertFlags(t *testing.T, want map[string]string) {
	t.Helper()
	flags := NewSuperviseCmd().Flags()
	for name, value := range want {
		if got := flags.Lookup(name).Value.String(); got != value {
			t.Errorf("--%s = %q, want %q", name, got, value)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	if err := fastSupervisor(t.TempDir(), io.Discard).validate(); err != nil {
		t.Errorf("valid timings refused: %v", err)
	}
	for name, change := range map[string]func(*Supervisor){
		"zero poll interval":     func(s *Supervisor) { s.PollInterval = 0 },
		"negative stop grace":    func(s *Supervisor) { s.StopGrace = -time.Second },
		"min delay above max":    func(s *Supervisor) { s.RestartDelayMin = time.Hour },
		"zero wait interval":     func(s *Supervisor) { s.WaitInterval = 0 },
		"zero restart delay min": func(s *Supervisor) { s.RestartDelayMin = 0 },
	} {
		s := fastSupervisor(t.TempDir(), io.Discard)
		change(s)
		if err := s.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSeconds(t *testing.T) {
	t.Parallel()
	if got := seconds(5 * time.Second); got != "5s" {
		t.Errorf("seconds(5s) = %q", got)
	}
	if got := seconds(160 * time.Second); got != "160s" {
		t.Errorf("seconds(160s) = %q", got)
	}
	if got := seconds(100 * time.Millisecond); got != "0.1s" {
		t.Errorf("seconds(100ms) = %q", got)
	}
}
