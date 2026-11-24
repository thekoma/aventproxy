// Package supervise keeps the bridge running inside the add-on container. It
// took over from the add-on's run.sh so that the image needs no shell and no
// OS packages (issue #124): it finds the config the integration wrote, runs
// `avent-webrtc-bridge addon` on it as a child process, restarts it when it
// exits or its config changes, and stops it on SIGTERM.
package supervise

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const (
	configPattern = "philips_avent_bridge_*.json"
	legacyConfig  = "philips_avent_bridge.json"
)

// ErrNoConfig ends the first start when there is no config and the
// supervisor was not asked to wait for one.
var ErrNoConfig = errors.New("no bridge config found")

// Supervisor holds the add-on's paths and timings. The command fills them from
// flags and the environment; the tests shrink the timings.
type Supervisor struct {
	ConfigDir     string   // where the integration writes philips_avent_bridge_*.json
	AddonConfig   string   // the add-on options, /data/options.json under Supervisor
	WaitForConfig bool     // at the first start, wait for a config instead of failing
	Bind          string   // RTSP bind address; when set it wins over the add-on option
	Bridge        []string // runs the bridge; the addon arguments are appended

	PollInterval    time.Duration // between config checks while the bridge runs
	WaitInterval    time.Duration // between looks for a config while there is none
	RestartDelayMin time.Duration
	RestartDelayMax time.Duration
	StableRun       time.Duration // a run this long resets the restart delay
	StopGrace       time.Duration // how long the bridge gets to stop before SIGKILL

	Out io.Writer // the supervisor's own lines; the bridge writes to stdout and stderr
}

// NewSuperviseCmd returns the command the add-on image runs as its entrypoint.
func NewSuperviseCmd() *cobra.Command {
	s := &Supervisor{Out: os.Stdout}
	cmd := &cobra.Command{
		Use:   "supervise",
		Short: "Run the add-on bridge on the integration's config and keep it running",
		Long: `Run "addon" on the bridge config the Home Assistant integration writes, and keep it running.

The add-on image runs this as its entrypoint. It takes the newest
philips_avent_bridge_*.json in --config-dir, else philips_avent_bridge.json,
else --addon-config when it holds signing_key and sid. It runs the bridge on it
as a child process, restarts it whenever it exits (after a delay that grows
while it keeps failing) or its config changes, and stops it on SIGTERM.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := s.validate(); err != nil {
				return err
			}
			cmd.SilenceUsage = true // from here on an error is not a usage mistake
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("find the bridge binary: %w", err)
			}
			s.Bridge = []string{exe}
			return s.runUntilSignalled()
		},
	}
	f := cmd.Flags()
	f.StringVar(&s.ConfigDir, "config-dir", envOr("CONFIG_DIR", "/config"),
		"Directory the integration writes its bridge config to (env CONFIG_DIR)")
	f.StringVar(&s.AddonConfig, "addon-config", envOr("ADDON_CONFIG", "/data/options.json"),
		"Add-on options file (env ADDON_CONFIG)")
	f.BoolVar(&s.WaitForConfig, "wait-for-config", os.Getenv("WAIT_FOR_CONFIG") == "true",
		"Wait for a config at the first start instead of failing (env WAIT_FOR_CONFIG=true)")
	f.StringVar(&s.Bind, "bind", os.Getenv("RTSP_BIND_ADDRESS"),
		"RTSP bind address, over the add-on option rtsp_bind_address; localhost when neither is set (env RTSP_BIND_ADDRESS)")
	f.DurationVar(&s.PollInterval, "poll-interval", 10*time.Second, "How often the running bridge's config is checked")
	f.DurationVar(&s.WaitInterval, "wait-interval", 5*time.Second, "How often to look for a config while there is none")
	f.DurationVar(&s.RestartDelayMin, "restart-delay-min", 5*time.Second, "Delay before restarting a bridge that exited")
	f.DurationVar(&s.RestartDelayMax, "restart-delay-max", 300*time.Second, "The delay doubles up to this while the bridge keeps failing")
	f.DurationVar(&s.StableRun, "stable-run", 60*time.Second, "A run this long resets the delay")
	f.DurationVar(&s.StopGrace, "stop-grace", 8*time.Second, "Time the bridge gets to stop before it is killed")
	return cmd
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (s *Supervisor) validate() error {
	for _, d := range []struct {
		flag  string
		value time.Duration
	}{
		{"poll-interval", s.PollInterval},
		{"wait-interval", s.WaitInterval},
		{"restart-delay-min", s.RestartDelayMin},
		{"restart-delay-max", s.RestartDelayMax},
		{"stable-run", s.StableRun},
		{"stop-grace", s.StopGrace},
	} {
		if d.value <= 0 {
			return fmt.Errorf("--%s must be positive, got %s", d.flag, d.value)
		}
	}
	if s.RestartDelayMin > s.RestartDelayMax {
		return fmt.Errorf("--restart-delay-min %s is above --restart-delay-max %s", s.RestartDelayMin, s.RestartDelayMax)
	}
	return nil
}

// runUntilSignalled runs s until SIGTERM, which is how Supervisor stops the
// add-on, or SIGINT.
func (s *Supervisor) runUntilSignalled() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return s.Run(ctx)
}

// Run supervises the bridge until ctx ends, then stops it and returns nil. It
// restarts the bridge on any exit status rather than ending, because exiting
// and relying on Supervisor to bring the add-on back left it stopped after two
// Home Assistant restarts in quick succession (issue #73). The restart waits a
// delay that doubles while the bridge keeps failing, so that a permanent
// failure does not hit the Tuya API every few seconds. The config is resolved
// again on every start and watched while the bridge runs.
func (s *Supervisor) Run(ctx context.Context) error {
	path, err := s.waitForConfig(ctx, !s.WaitForConfig)
	if err != nil {
		return err
	}
	if path == "" {
		return s.shutdown()
	}
	s.warnOnExtraConfigs()
	bind := s.rtspBind()
	s.logBanner(bind)

	delay := s.RestartDelayMin
	for {
		started := time.Now()
		switch result, why := s.runBridge(ctx, path, bind); result {
		case stopped:
			return nil
		case configChanged:
			delay = s.RestartDelayMin
		case exited:
			if time.Since(started) >= s.StableRun {
				delay = s.RestartDelayMin
			}
			s.logf("Bridge %s, restarting in %s...", why, seconds(delay))
			if !sleep(ctx, delay) {
				return s.shutdown()
			}
			delay = min(2*delay, s.RestartDelayMax)
		}
		// The bridge has run once, so a config that is gone is waited for
		// even without WaitForConfig.
		if path, _ = s.waitForConfig(ctx, false); path == "" {
			return s.shutdown()
		}
	}
}

func (s *Supervisor) shutdown() error {
	s.logf("Stopping bridge...")
	return nil
}

type outcome int

const (
	exited        outcome = iota // the bridge ended on its own
	configChanged                // the bridge was stopped to pick up a new config
	stopped                      // ctx ended and the bridge was stopped
)

// runBridge runs the bridge on path until it exits, its config changes or ctx
// ends. When the bridge exited on its own, why says how.
func (s *Supervisor) runBridge(ctx context.Context, path, bind string) (result outcome, why string) {
	s.logf("Using bridge config: %s", path)
	sum, readable := fileSum(path)
	cmd := exec.Command(s.Bridge[0], append(slices.Clone(s.Bridge[1:]), s.bridgeArgs(path, bind)...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return exited, fmt.Sprintf("could not start: %v", err)
	}
	s.logf("Bridge started (pid %d)", cmd.Process.Pid)
	done := make(chan struct{})
	go func() {
		// The exit status is read from cmd.ProcessState. Wait has nothing
		// else to report: stdout and stderr are files, so it copies nothing.
		_ = cmd.Wait()
		close(done)
	}()

	poll := time.NewTicker(s.PollInterval)
	defer poll.Stop()
	for {
		select {
		case <-done:
			return exited, fmt.Sprintf("exited with status %d", exitStatus(cmd.ProcessState))
		case <-ctx.Done():
			s.logf("Stopping bridge...")
			s.stopBridge(cmd, done)
			return stopped, ""
		case <-poll.C:
			if reason := s.configChange(path, sum, readable); reason != "" {
				s.logf("%s", reason)
				s.stopBridge(cmd, done)
				return configChanged, ""
			}
		}
	}
}

// stopBridge asks the bridge to stop, and kills it if it has not within
// StopGrace: a bridge that hangs must not hold up a restart or Supervisor's
// stop.
func (s *Supervisor) stopBridge(cmd *exec.Cmd, done <-chan struct{}) {
	// An error here means the bridge has exited already, and done is closing.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
		return
	case <-time.After(s.StopGrace):
	}
	s.logf("Bridge did not stop in time, killing it")
	_ = cmd.Process.Kill()
	<-done
}

// configChange says why the running bridge must restart for its config, or
// returns "". Another integration file takes over only once the current one is
// gone, or when the bridge runs on the add-on options: with two config entries
// both files are valid, and following whichever was written last would restart
// the stream each time the other one is rewritten. A re-added integration (new
// entry id) removes the old file, so it is still picked up (issues #52, #159).
// The content is compared, not the mtime: the integration writes the same bytes
// when nothing changed, and that must not restart the stream (issue #73).
func (s *Supervisor) configChange(path string, sum [sha256.Size]byte, readable bool) string {
	latest := s.findBridgeConfig()
	if latest != "" && latest != path && (path == s.AddonConfig || !isFile(path)) {
		return fmt.Sprintf("Bridge config is now %s, restarting bridge...", latest)
	}
	if now, ok := fileSum(path); ok && (!readable || now != sum) {
		return "Config changed, restarting bridge..."
	}
	return ""
}

// waitForConfig returns the config to run, waiting until there is one. It logs
// the wait once, so that a long one does not flood the log. With failFast it
// returns ErrNoConfig instead of waiting. It returns "" when ctx ends.
func (s *Supervisor) waitForConfig(ctx context.Context, failFast bool) (string, error) {
	announced := false
	for ctx.Err() == nil { // no new bridge once a stop has been asked for
		if path := s.resolveConfig(); path != "" {
			if announced {
				s.logf("Config found!")
			}
			return path, nil
		}
		if failFast {
			return "", ErrNoConfig
		}
		if !announced {
			s.logf("Waiting for bridge config from HA integration...")
			announced = true
		}
		sleep(ctx, s.WaitInterval)
	}
	return "", nil
}

// resolveConfig returns the newest integration config, else the add-on options
// when they carry credentials, else "".
func (s *Supervisor) resolveConfig() string {
	if path := s.findBridgeConfig(); path != "" {
		return path
	}
	// /data/options.json always exists under Supervisor, so its presence says
	// nothing: only credentials the user filled in by hand make it a config.
	opts := s.addonOptions()
	if option(opts, "signing_key") != "" && option(opts, "sid") != "" {
		return s.AddonConfig
	}
	return ""
}

// findBridgeConfig returns the newest integration config, else the legacy
// single file, else "".
func (s *Supervisor) findBridgeConfig() string {
	if configs := s.integrationConfigs(); len(configs) > 0 {
		return configs[0]
	}
	if legacy := filepath.Join(s.ConfigDir, legacyConfig); isFile(legacy) {
		return legacy
	}
	return ""
}

// integrationConfigs lists the integration's config files, newest first. The
// integration writes one per config entry, and a leftover from a deleted entry
// used to win just by sorting first, which had the bridge streaming an old
// session and camera id (issue #52).
func (s *Supervisor) integrationConfigs() []string {
	entries, err := os.ReadDir(s.ConfigDir)
	if err != nil {
		return nil // a /config that cannot be read holds no config yet
	}
	type file struct {
		path  string
		mtime time.Time
	}
	var files []file
	for _, e := range entries {
		if ok, _ := filepath.Match(configPattern, e.Name()); !ok {
			continue
		}
		path := filepath.Join(s.ConfigDir, e.Name())
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			files = append(files, file{path, info.ModTime()})
		}
	}
	// Stable, so that files written in the same instant keep ReadDir's name
	// order, as `ls -t` did.
	slices.SortStableFunc(files, func(a, b file) int { return b.mtime.Compare(a.mtime) })
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.path
	}
	return paths
}

func (s *Supervisor) warnOnExtraConfigs() {
	configs := s.integrationConfigs()
	if len(configs) < 2 {
		return
	}
	s.logf("WARNING: %d bridge config files found in %s, using the newest one:", len(configs), s.ConfigDir)
	for _, path := range configs {
		s.logf("  %s", path)
	}
	s.logf("The integration deletes the others on its next start. If they stay, remove the ones")
	s.logf("that do not belong to your current integration entry by hand.")
}

// rtspBind says where the RTSP server listens. It has no authentication, so it
// stays on loopback unless the user opens it on purpose: Home Assistant shares
// the host network with the add-on and reaches it at rtsp://localhost:<port>.
// Bind (RTSP_BIND_ADDRESS, for docker-compose setups that have no options.json)
// wins over the add-on option rtsp_bind_address, which wins over localhost.
func (s *Supervisor) rtspBind() string {
	if s.Bind != "" {
		return s.Bind
	}
	if bind := option(s.addonOptions(), "rtsp_bind_address"); bind != "" {
		return bind
	}
	return "localhost"
}

func (s *Supervisor) logBanner(bind string) {
	s.logf("==============================")
	s.logf("Philips Avent WebRTC Bridge")
	s.logf("RTSP bind: %s", bind)
	s.logf("==============================")
	switch bind {
	case "localhost", "127.0.0.1", "::1", "127.0.0.1,::1":
	default:
		s.logf("WARNING: the RTSP server listens on '%s' and has no password:", bind)
		s.logf("anyone who can reach this port can watch and hear the camera.")
	}
}

// bridgeArgs builds the addon command line. An option reaches the bridge only
// when options.json has it: the Dev card alone defines the issue #72 knobs, and
// a card without an option keeps the bridge's default. A false must still get
// through, so only a missing or null value is left out.
func (s *Supervisor) bridgeArgs(path, bind string) []string {
	args := []string{"addon", "--config", path, "--bind", bind}
	opts := s.addonOptions()
	// A bool flag takes its value only after "=".
	if v := option(opts, "speaker_off"); v != "" {
		args = append(args, "--speaker-off="+v)
	}
	for _, o := range []struct{ key, flag string }{
		{"webrtc_audio", "--webrtc-audio"},
		{"cloud_transport", "--cloud-transport"},
		{"talkback_mode", "--talkback-mode"},
	} {
		if v := option(opts, o.key); v != "" {
			args = append(args, o.flag, v)
		}
	}
	return args
}

// addonOptions reads the add-on options. A missing file reads as no options,
// as docker-compose setups have none; so does one that is not a JSON object,
// which Supervisor never writes and run.sh's jq calls skipped the same way.
func (s *Supervisor) addonOptions() map[string]any {
	data, err := os.ReadFile(s.AddonConfig)
	if err != nil {
		return nil
	}
	var opts map[string]any
	if err := json.Unmarshal(data, &opts); err != nil {
		return nil
	}
	return opts
}

// option renders an add-on option as a command-line value: "" when it is
// missing, null or an empty string.
func option(opts map[string]any, key string) string {
	switch v := opts[key].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		text, _ := json.Marshal(v) // a value decoded from JSON always encodes
		return string(text)
	}
}

// fileSum hashes a config file to notice a change; ok is false when the file
// cannot be read.
func fileSum(path string) (sum [sha256.Size]byte, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sum, false
	}
	return sha256.Sum256(data), true
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// exitStatus reports a status the way a shell does: 128 plus the signal for a
// bridge that a signal ended.
func exitStatus(state *os.ProcessState) int {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}

// sleep waits for d and reports false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// seconds prints a delay the way the add-on log always has: 5s, 160s, 300s.
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}

func (s *Supervisor) logf(format string, args ...any) {
	fmt.Fprintf(s.Out, format+"\n", args...)
}
