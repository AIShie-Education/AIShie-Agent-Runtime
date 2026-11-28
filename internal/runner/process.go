package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// ProcessOptions configure a Manager. URL selects an external runner and no
// child process. Otherwise Bin runs Entry on 127.0.0.1:Port.
type ProcessOptions struct {
	URL          string
	Port         int
	Bin          string
	Entry        string
	Dir          string
	HTTPClient   *http.Client
	Log          *slog.Logger
	StartTimeout time.Duration
	StopTimeout  time.Duration
	RestartMin   time.Duration
	RestartMax   time.Duration
}

func (o ProcessOptions) defaults() ProcessOptions {
	if o.Port == 0 {
		o.Port = 3001
	}
	if o.Bin == "" {
		o.Bin = "node"
	}
	if o.Entry == "" {
		o.Entry = "runner/dist/index.js"
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = 15 * time.Second
	}
	if o.StopTimeout <= 0 {
		o.StopTimeout = 5 * time.Second
	}
	if o.RestartMin <= 0 {
		o.RestartMin = 250 * time.Millisecond
	}
	if o.RestartMax <= 0 {
		o.RestartMax = 5 * time.Second
	}
	return o
}

// Manager owns a child runner, or points at an external one. A child that
// exits unexpectedly is restarted with bounded exponential backoff.
type Manager struct {
	opts   ProcessOptions
	client *Client
	cancel context.CancelFunc
	done   chan struct{}
}

// Start starts and health-checks a child, or health-checks URL when it names
// an external runner. The child inherits no conversational data or secrets.
func Start(ctx context.Context, options ProcessOptions) (*Manager, error) {
	o := options.defaults()
	if o.URL != "" {
		m := &Manager{opts: o, client: NewClient(o.URL, o.HTTPClient), done: closedChan()}
		hctx, cancel := context.WithTimeout(ctx, o.StartTimeout)
		defer cancel()
		if err := m.client.Healthy(hctx); err != nil {
			return nil, err
		}
		return m, nil
	}
	if o.Port < 1 || o.Port > 65535 {
		return nil, fmt.Errorf("runner: invalid port %d", o.Port)
	}
	if _, err := exec.LookPath(o.Bin); err != nil {
		return nil, fmt.Errorf("runner: %s: %w", o.Bin, err)
	}
	o.URL = fmt.Sprintf("http://127.0.0.1:%d", o.Port)
	runCtx, cancel := context.WithCancel(ctx)
	m := &Manager{opts: o, client: NewClient(o.URL, o.HTTPClient), cancel: cancel, done: make(chan struct{})}
	cmd, wait, err := m.startChild(runCtx)
	if err != nil {
		cancel()
		close(m.done)
		return nil, err
	}
	go m.supervise(runCtx, cmd, wait)
	return m, nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Client is the manager's runner client.
func (m *Manager) Client() *Client { return m.client }

// URL is the internal or external runner's base URL.
func (m *Manager) URL() string { return m.opts.URL }

// Close gracefully stops a managed child and waits for its supervisor. An
// external runner is left running.
func (m *Manager) Close(ctx context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type child struct {
	cmd  *exec.Cmd
	wait chan error
	done chan struct{}
}

func (m *Manager) startChild(ctx context.Context) (*child, chan error, error) {
	cmd := exec.Command(m.opts.Bin, m.opts.Entry) // #nosec G204 -- operator-selected local runner executable and entrypoint.
	cmd.Dir = m.opts.Dir
	cmd.Env = append(os.Environ(), "RUNNER_PORT="+strconv.Itoa(m.opts.Port))
	// Runner output may contain provider text. Keep it out of the Go log; the
	// supervisor logs process lifecycle events only.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("runner: start: %w", err)
	}
	ch := &child{cmd: cmd, wait: make(chan error, 1), done: make(chan struct{})}
	go func() {
		ch.wait <- cmd.Wait()
		close(ch.done)
	}()
	hctx, cancel := context.WithTimeout(ctx, m.opts.StartTimeout)
	defer cancel()
	if err := m.waitHealthy(hctx, ch.wait); err != nil {
		m.stopChild(ch)
		return nil, nil, err
	}
	return ch, ch.wait, nil
}

func (m *Manager) waitHealthy(ctx context.Context, exited <-chan error) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		err := m.client.Healthy(ctx)
		if err == nil {
			return nil
		}
		last = err
		select {
		case err := <-exited:
			if err == nil {
				return errors.New("runner: exited before becoming healthy")
			}
			return fmt.Errorf("runner: exited before becoming healthy: %w", err)
		case <-ctx.Done():
			if last != nil {
				return last
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) supervise(ctx context.Context, first *child, _ chan error) {
	defer close(m.done)
	current := first
	backoff := m.opts.RestartMin
	for {
		select {
		case err := <-current.wait:
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				m.opts.Log.Error("runner exited unexpectedly", "err", "exit status 0")
			} else {
				m.opts.Log.Error("runner exited unexpectedly", "err", err)
			}
		case <-ctx.Done():
			m.stopChild(current)
			return
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, m.opts.RestartMax)
		next, _, err := m.startChild(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.opts.Log.Error("runner could not be restarted", "err", err)
			// Keep the lifecycle loop alive with an already-ended child so the
			// same bounded backoff is applied to the next attempt.
			next = &child{wait: closedError(errors.New("runner is down"))}
		}
		current = next
	}
}

func closedError(err error) chan error {
	ch := make(chan error, 1)
	ch <- err
	return ch
}

func (m *Manager) stopChild(ch *child) {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return
	}
	_ = ch.cmd.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(m.opts.StopTimeout)
	defer timer.Stop()
	select {
	case <-ch.done:
		return
	case <-timer.C:
		_ = ch.cmd.Process.Kill()
		<-ch.done
	}
}
