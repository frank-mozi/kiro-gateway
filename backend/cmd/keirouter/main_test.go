package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type fakeTrayLifecycle struct {
	runErr  error
	stopped chan struct{}
	once    sync.Once
}

func newFakeTrayLifecycle(runErr error) *fakeTrayLifecycle {
	return &fakeTrayLifecycle{runErr: runErr, stopped: make(chan struct{})}
}

func (f *fakeTrayLifecycle) Run(ctx context.Context) error {
	if f.runErr != nil {
		return f.runErr
	}
	select {
	case <-f.stopped:
		return nil
	case <-ctx.Done():
		return nil
	}
}

func (f *fakeTrayLifecycle) Stop() {
	f.once.Do(func() { close(f.stopped) })
}

func TestPrintUsage(t *testing.T) {
	err := run([]string{"help"})
	assert.NoError(t, err)

	err = run([]string{"--help"})
	assert.NoError(t, err)

	err = run([]string{"-h"})
	assert.NoError(t, err)

	file, err := os.CreateTemp(t.TempDir(), "usage")
	if !assert.NoError(t, err) {
		return
	}
	printUsage(file)
	assert.NoError(t, file.Close())
	usage, err := os.ReadFile(file.Name())
	assert.NoError(t, err)
	assert.Contains(t, string(usage), "Kiro Gateway")
	assert.Contains(t, string(usage), "kiro-gateway [command]")
	assert.Contains(t, string(usage), "default with no arguments")
	assert.Contains(t, string(usage), "Windows also supports double-click")
	assert.NotContains(t, string(usage), "Start the server (default")
}

func TestPortableCommandRejectsInvalidPort(t *testing.T) {
	for _, port := range []string{"0", "1023", "65536"} {
		t.Run(port, func(t *testing.T) {
			err := run([]string{"portable", "--port", port, "--no-browser"})
			assert.EqualError(t, err, "portable port must be between 1024 and 65535")
		})
	}
}

func TestNoArgumentsUsePortable(t *testing.T) {
	if os.Getenv("KEIROUTER_TEST_DEFAULT_PORTABLE") == "1" {
		executable, err := os.Executable()
		if !assert.NoError(t, err) {
			return
		}
		root, err := filepath.EvalSymlinks(filepath.Dir(executable))
		if !assert.NoError(t, err) {
			return
		}
		err = run(nil)
		var pathError *os.PathError
		if assert.ErrorAs(t, err, &pathError) {
			assert.Equal(t, filepath.Join(root, ".portable.lock"), pathError.Path)
		}
		return
	}

	root := t.TempDir()
	executable, err := os.Executable()
	if !assert.NoError(t, err) {
		return
	}
	binary, err := os.ReadFile(executable)
	if !assert.NoError(t, err) {
		return
	}
	copyPath := filepath.Join(root, filepath.Base(executable))
	if !assert.NoError(t, os.WriteFile(copyPath, binary, 0700)) {
		return
	}
	// Force portable mode to fail before opening a port or launching a browser.
	if !assert.NoError(t, os.Mkdir(filepath.Join(root, ".portable.lock"), 0700)) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, copyPath, "-test.run=^TestNoArgumentsUsePortable$")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"KEIROUTER_TEST_DEFAULT_PORTABLE=1",
		"SystemRoot=" + os.Getenv("SystemRoot"),
		"HOME=" + root,
		"USERPROFILE=" + root,
		"KEIROUTER_SERVER__PORT=invalid",
	}
	output, err := cmd.CombinedOutput()
	assert.NoError(t, err, "%s", output)
}

func TestRunVersion(t *testing.T) {
	err := run([]string{"version"})
	assert.NoError(t, err)

	err = run([]string{"-version"})
	assert.NoError(t, err)
}

func TestUnknownCommand(t *testing.T) {
	err := run([]string{"nonexistent-command-xyz"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

func TestRunTrayLifecycleStopsTrayWhenServerFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tray := newFakeTrayLifecycle(nil)
	wantErr := errors.New("listen tcp: address already in use")

	err := runTrayLifecycle(ctx, cancel, func(context.Context) error {
		return wantErr
	}, tray)

	assert.ErrorIs(t, err, wantErr)
	select {
	case <-tray.stopped:
	default:
		t.Fatal("tray was not stopped after the server failed")
	}
}

func TestRunTrayLifecycleCancelsServerWhenTrayFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wantErr := errors.New("tray event loop failed")
	tray := newFakeTrayLifecycle(wantErr)
	serverStopped := make(chan struct{})

	err := runTrayLifecycle(ctx, cancel, func(ctx context.Context) error {
		<-ctx.Done()
		close(serverStopped)
		return nil
	}, tray)

	assert.ErrorIs(t, err, wantErr)
	select {
	case <-serverStopped:
	default:
		t.Fatal("server was not cancelled after the tray failed")
	}
}
