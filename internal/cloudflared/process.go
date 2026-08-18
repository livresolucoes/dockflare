package cloudflared

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/livresolucoes/dockflare/internal/logger"
)

const (
	stopTimeout   = 10 * time.Second
	reloadTimeout = 5 * time.Second
	crashBackoff  = 3 * time.Second
	maxCrashRetry = 5
)

// Process manages the cloudflared subprocess lifecycle.
type Process struct {
	binaryPath string
	token      string
	cmd        *exec.Cmd
	mu         sync.Mutex
	log        *logger.Logger
	crashCh    chan struct{}
}

func New(binaryPath, token string, log *logger.Logger) *Process {
	return &Process{
		binaryPath: binaryPath,
		token:      token,
		log:        log,
		crashCh:    make(chan struct{}, 1),
	}
}

// CrashCh returns a channel that receives a signal when cloudflared exits
// unexpectedly after exhausting all restart retries.
func (p *Process) CrashCh() <-chan struct{} {
	return p.crashCh
}

// Running reports whether the subprocess is currently up.
func (p *Process) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd != nil && p.cmd.Process != nil
}

// PID returns the subprocess PID, or 0 when it is not running.
func (p *Process) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Start launches cloudflared with the generated config. Must not be called
// while the process is already running.
func (p *Process) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.start(ctx)
}

func (p *Process) start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, p.binaryPath, "tunnel", "run", "--token", p.token)
	// Forward cloudflared output to our logger.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("cloudflared stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("cloudflared stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting cloudflared: %w", err)
	}
	p.cmd = cmd
	p.log.Info("cloudflared started (pid %d)", cmd.Process.Pid)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			p.log.Info("[cloudflared] %s", scanner.Text())
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			p.log.Info("[cloudflared] %s", scanner.Text())
		}
	}()
	go p.watchCrash(ctx, 0)
	return nil
}

// watchCrash waits for the subprocess to exit and restarts it with backoff.
// retry counts restarts attempted so far; stops after maxCrashRetry.
func (p *Process) watchCrash(ctx context.Context, retry int) {
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd == nil {
		return
	}

	err := cmd.Wait()

	// If context was cancelled, this is an intentional stop — not a crash.
	select {
	case <-ctx.Done():
		return
	default:
	}

	p.mu.Lock()
	// If cmd was replaced (Reload/Stop called intentionally), also not a crash.
	if p.cmd != cmd {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	if retry >= maxCrashRetry {
		p.log.Error("cloudflared exited (%v) and exceeded max retries (%d), giving up", err, maxCrashRetry)
		select {
		case p.crashCh <- struct{}{}:
		default:
		}
		return
	}

	p.log.Warn("cloudflared exited unexpectedly (%v), restarting in %s (attempt %d/%d)",
		err, crashBackoff, retry+1, maxCrashRetry)
	select {
	case <-time.After(crashBackoff):
	case <-ctx.Done():
		return
	}

	p.mu.Lock()
	startErr := p.start(ctx)
	p.mu.Unlock()
	if startErr != nil {
		p.log.Error("restarting cloudflared: %v", startErr)
	}
}

// Reload performs a graceful reload by stopping and restarting the subprocess.
func (p *Process) Reload(ctx context.Context) error {
	p.log.Info("Reloading cloudflared")
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.terminate(); err != nil {
		p.log.Warn("terminating cloudflared for reload: %v", err)
	}
	return p.start(ctx)
}

// Stop sends SIGTERM and waits for the process to exit.
func (p *Process) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminate()
}

func (p *Process) terminate() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	old := p.cmd
	p.cmd = nil // mark as intentional stop before signalling

	if err := old.Process.Signal(syscall.SIGTERM); err != nil {
		old.Process.Kill()
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- old.Wait() }()

	select {
	case <-done:
	case <-time.After(stopTimeout):
		old.Process.Kill()
		<-done
	}
	return nil
}
