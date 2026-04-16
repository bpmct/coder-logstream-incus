// Package main implements coder-logstream-incus core log streaming logic.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"cdr.dev/slog/v3"
	"github.com/fatih/color"
	"github.com/google/uuid"
	incusclient "github.com/lxc/incus/v6/client"
	incusapi "github.com/lxc/incus/v6/shared/api"
	"golang.org/x/mod/semver"

	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/agentsdk"

	// Never remove this. Certificates are not bundled as part
	// of the binary, so this is necessary for all TLS connections.
	_ "github.com/breml/rootcerts"
)

// sourceUUID is the stable UUID identifying the "Incus" log source.
var sourceUUID = uuid.MustParse("a3bb5c89-7f3c-4f58-b6d3-a3c5e7b1f0d2")

// cloudInitPollInterval is how often we poll for new cloud-init log lines.
const cloudInitPollInterval = 2 * time.Second

// cloudInitLogPath is the path to cloud-init's combined output log inside the VM.
const cloudInitLogPath = "/var/log/cloud-init-output.log"

// cloudInitResultPath signals that cloud-init has finished.
const cloudInitResultPath = "/run/cloud-init/result.json"

type incusLogStreamerOptions struct {
	coderURL   *url.URL
	socketPath string
	project    string
	logger     slog.Logger
	// maxRetries controls log sender retry limit (0 → 15).
	maxRetries int
}

// incusLogStreamer watches an Incus project for VMs that carry a Coder agent
// token in their config and streams their console + cloud-init logs.
type incusLogStreamer struct {
	opts       incusLogStreamerOptions
	incus      incusclient.InstanceServer
	errChan    chan error
	cancelFunc context.CancelFunc

	mu     sync.Mutex
	active map[string]context.CancelFunc // instance name → cancel
	done   map[string]struct{}            // instances that completed streaming successfully
}

func newIncusLogStreamer(ctx context.Context, opts incusLogStreamerOptions) (*incusLogStreamer, error) {
	if opts.maxRetries == 0 {
		opts.maxRetries = 15
	}
	if opts.project == "" {
		opts.project = "default"
	}

	raw, err := incusclient.ConnectIncusUnix(opts.socketPath, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to incus socket: %w", err)
	}

	srv := raw.UseProject(opts.project)

	ctx, cancel := context.WithCancel(ctx)
	s := &incusLogStreamer{
		opts:       opts,
		incus:      srv,
		errChan:    make(chan error, 8),
		cancelFunc: cancel,
		active:     make(map[string]context.CancelFunc),
		done:       make(map[string]struct{}),
	}

	go s.watchLoop(ctx)
	return s, nil
}

// watchLoop polls Incus for instances every 5s, starting/stopping
// streaming goroutines as instances appear and disappear.
func (s *incusLogStreamer) watchLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Run immediately on start.
	s.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

// reconcile fetches the current instance list and starts/stops streamers.
func (s *incusLogStreamer) reconcile(ctx context.Context) {
	instances, err := s.incus.GetInstances(incusapi.InstanceTypeAny)
	if err != nil {
		s.opts.logger.Error(ctx, "list instances", slog.Error(err))
		return
	}

	current := make(map[string]string) // name → token
	for _, inst := range instances {
		token := agentToken(inst)
		if token != "" {
			current[inst.Name] = token
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Start streamers for new instances (skip already-completed ones).
	for name, token := range current {
		if _, done := s.done[name]; done {
			continue // already streamed to completion, no need to restart
		}
		if _, ok := s.active[name]; !ok {
			iCtx, iCancel := context.WithCancel(ctx)
			s.active[name] = iCancel
			go func(iCtx context.Context, name, token string) {
				completed := s.streamInstance(iCtx, name, token)
				// Remove from active. If completed normally, mark done so we
				// don't restart on the next reconcile. If not completed (e.g.
				// 401 during build, context cancelled), omit from done so
				// reconcile can retry.
				s.mu.Lock()
				delete(s.active, name)
				if completed {
					s.done[name] = struct{}{}
				}
				s.mu.Unlock()
			}(iCtx, name, token)
		}
	}

	// Cancel streamers for gone instances and clean up done map.
	for name, cancel := range s.active {
		if _, ok := current[name]; !ok {
			cancel()
			delete(s.active, name)
		}
	}
	for name := range s.done {
		if _, ok := current[name]; !ok {
			delete(s.done, name)
		}
	}
}

// agentToken extracts the Coder agent token from an Incus instance config.
// It checks user.coder-agent-token (Incus user metadata) and
// environment.CODER_AGENT_TOKEN (cloud-init environment key).
func agentToken(inst incusapi.Instance) string {
	if t, ok := inst.Config["user.coder-agent-token"]; ok && t != "" {
		return t
	}
	if t, ok := inst.ExpandedConfig["user.coder-agent-token"]; ok && t != "" {
		return t
	}
	if t, ok := inst.Config["environment.CODER_AGENT_TOKEN"]; ok && t != "" {
		return t
	}
	if t, ok := inst.ExpandedConfig["environment.CODER_AGENT_TOKEN"]; ok && t != "" {
		return t
	}
	return ""
}

// streamInstance is the per-VM goroutine that sends logs to Coder.
// It returns true if cloud-init streaming completed normally, false if it
// exited early (context cancelled, 401 during build, etc.).
func (s *incusLogStreamer) streamInstance(ctx context.Context, name, token string) bool {
	logger := s.opts.logger.With(slog.F("instance", name))
	logger.Info(ctx, "starting log stream for instance")

	client := agentsdk.New(s.opts.coderURL, agentsdk.WithFixedToken(token))
	client.SDK.SetLogger(logger)

	// The workspace token may not be authorized until the build job completes.
	// Retry the connection with 10s backoff until successful or context cancelled.
	var logDest agentsdk.LogDest
	var closeConn func()

	for {
		if ctx.Err() != nil {
			return false
		}

		// Fetch build info to determine server capabilities.
		// The role query parameter was added in Coder v2.31.0.
		buildInfo, buildInfoErr := client.SDK.BuildInfo(ctx)

		supportsRole := buildInfoErr == nil && versionAtLeast(buildInfo.Version, "v2.31.0")
		var connErr error
		if supportsRole {
			raw, _, err := client.ConnectRPC28WithRole(ctx, "logstream-incus")
			if err == nil {
				logDest = raw
				closeConn = func() { _ = raw.DRPCConn().Close() }
			} else {
				connErr = err
			}
		} else {
			raw, err := client.ConnectRPC20(ctx)
			if err == nil {
				logDest = raw
				closeConn = func() { _ = raw.DRPCConn().Close() }
			} else {
				connErr = err
			}
		}

		if connErr == nil {
			break
		}

		logger.Warn(ctx, "connect to coder agent API (retrying in 10s)", slog.Error(connErr))
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Second):
		}
	}
	defer closeConn()

	// Register log source (best-effort, non-fatal).
	_, err := client.PostLogSource(ctx, agentsdk.PostLogSourceRequest{
		ID:          sourceUUID,
		Icon:        "/icon/container.svg",
		DisplayName: "Incus",
	})
	if err != nil {
		logger.Error(ctx, "post log source", slog.Error(err))
	}

	ls := agentsdk.NewLogSender(logger)
	sl := ls.GetScriptLogger(sourceUUID)

	gracefulCtx, gracefulCancel := context.WithCancel(context.Background())
	defer gracefulCancel()

	go func() {
		if err := ls.SendLoop(gracefulCtx, logDest); err != nil && ctx.Err() == nil {
			logger.Warn(gracefulCtx, "send loop exited", slog.Error(err))
		}
	}()

	sendLine := func(line string, level codersdk.LogLevel) {
		if err := sl.Send(ctx, agentsdk.Log{
			CreatedAt: time.Now(),
			Output:    line,
			Level:     level,
		}); err != nil && ctx.Err() == nil {
			logger.Warn(ctx, "send log line", slog.Error(err))
		}
	}

	// 1. Dump the console log buffer (one-shot; containers only — VMs don't support it).
	s.dumpConsoleLog(ctx, name, sendLine, logger)

	// 2. Tail cloud-init-output.log until done or context cancelled.
	completed := s.tailCloudInit(ctx, name, sendLine, logger)

	// Flush remaining logs gracefully.
	ls.Flush(sourceUUID)
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer flushCancel()
	if err := ls.WaitUntilEmpty(flushCtx); err != nil {
		logger.Warn(flushCtx, "wait for log queue", slog.Error(err))
	}

	logger.Info(ctx, "log stream finished for instance")
	return completed
}

// dumpConsoleLog reads the full console log buffer and sends each line.
// This only works for Incus containers; VMs use a different mechanism
// and will return an error which is silently ignored.
func (s *incusLogStreamer) dumpConsoleLog(ctx context.Context, name string, sendLine func(string, codersdk.LogLevel), logger slog.Logger) {
	rc, err := s.incus.GetInstanceConsoleLog(name, nil)
	if err != nil {
		// VMs don't support the console log buffer API — this is expected and not an error.
		logger.Debug(ctx, "get console log (skipping)", slog.Error(err))
		return
	}
	defer rc.Close()

	sendLine(newColor(color.Bold).Sprintf("=== Incus console log: %s ===", name), codersdk.LogLevelInfo)
	scanner := bufio.NewScanner(rc)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		sendLine(scanner.Text(), codersdk.LogLevelInfo)
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		logger.Warn(ctx, "scan console log", slog.Error(err))
	}
}

// tailCloudInit polls /var/log/cloud-init-output.log via the Incus file API,
// sending new lines every cloudInitPollInterval. It stops when:
//   - cloud-init finishes (/run/cloud-init/result.json exists), or
//   - context is cancelled, or
//   - the instance disappears.
//
// Returns true if cloud-init completed normally, false if context was cancelled.
func (s *incusLogStreamer) tailCloudInit(ctx context.Context, name string, sendLine func(string, codersdk.LogLevel), logger slog.Logger) bool {
	ticker := time.NewTicker(cloudInitPollInterval)
	defer ticker.Stop()

	var offset int64
	headerSent := false

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}

		// Check if cloud-init has completed.
		if s.fileExists(name, cloudInitResultPath) {
			// Do one final read before exiting.
			offset = s.readCloudInitChunk(ctx, name, offset, &headerSent, sendLine, logger)
			sendLine(newColor(color.FgGreen).Sprint("cloud-init: finished ✓"), codersdk.LogLevelInfo)
			return true
		}

		offset = s.readCloudInitChunk(ctx, name, offset, &headerSent, sendLine, logger)
	}
}

// readCloudInitChunk reads new content from cloud-init-output.log starting at
// offset and returns the new offset.
func (s *incusLogStreamer) readCloudInitChunk(ctx context.Context, name string, offset int64, headerSent *bool, sendLine func(string, codersdk.LogLevel), logger slog.Logger) int64 {
	rc, resp, err := s.incus.GetInstanceFile(name, cloudInitLogPath)
	if err != nil {
		// File may not exist yet — expected early in boot.
		if !strings.Contains(err.Error(), "No such file") &&
			!strings.Contains(err.Error(), "not found") &&
			!strings.Contains(err.Error(), "404") {
			logger.Debug(ctx, "get cloud-init log (not ready yet)", slog.Error(err))
		}
		return offset
	}
	defer rc.Close()

	if resp == nil {
		return offset
	}

	// Skip bytes already read (the Incus file API doesn't support range reads,
	// so we manually discard the already-sent portion).
	if offset > 0 {
		buf := make([]byte, 32*1024)
		var skipped int64
		for skipped < offset {
			need := offset - skipped
			if int64(len(buf)) < need {
				need = int64(len(buf))
			}
			n, err := rc.Read(buf[:need])
			skipped += int64(n)
			if err != nil {
				return offset
			}
		}
	}

	// Read remaining content.
	data, err := io.ReadAll(rc)
	if err != nil && len(data) == 0 {
		return offset
	}

	if len(data) == 0 {
		return offset
	}

	if !*headerSent {
		sendLine(newColor(color.Bold).Sprintf("=== cloud-init output: %s ===", name), codersdk.LogLevelInfo)
		*headerSent = true
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if ctx.Err() != nil {
			break
		}
		// Skip empty trailing line from Split.
		if line == "" {
			continue
		}
		sendLine(line, codersdk.LogLevelInfo)
	}

	return offset + int64(len(data))
}

// fileExists probes whether a path exists inside the instance via the file API.
func (s *incusLogStreamer) fileExists(name, path string) bool {
	rc, _, err := s.incus.GetInstanceFile(name, path)
	if err != nil {
		return false
	}
	_ = rc.Close()
	return true
}

// Close stops the streamer and all per-instance goroutines.
func (s *incusLogStreamer) Close() {
	s.cancelFunc()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.active {
		cancel()
	}
}

func newColor(value ...color.Attribute) *color.Color {
	c := color.New(value...)
	c.EnableColor()
	return c
}

// versionAtLeast returns true if version is a valid semver string and is
// greater than or equal to minimum.
func versionAtLeast(version, minimum string) bool {
	return semver.IsValid(version) && semver.Compare(version, minimum) >= 0
}
