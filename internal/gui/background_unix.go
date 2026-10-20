//go:build (linux && !android) || (darwin && !ios)

package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/unofficialpku/disk/internal/localfile"
)

const guiStartupFD = "PKUDISK_GUI_STARTUP_FD"

type guiStartup struct {
	Started bool   `json:"started"`
	Error   string `json:"error,omitempty"`
}

func RunMode(ctx context.Context, opts Options, foreground, startedFromDesktop bool) error {
	if !foreground && !startedFromDesktop {
		return startBackgroundGUI(ctx, opts.Output)
	}
	notify := guiStartupNotifier()
	opts.Ready = func(started bool) { notify(guiStartup{Started: started}) }
	err := Run(ctx, opts)
	if err != nil {
		notify(guiStartup{Error: err.Error()})
	}
	return err
}

// Only the re-executed child receives this pipe. It never carries a browser
// ticket or session credential, and is closed before the desktop service runs.
func guiStartupNotifier() func(guiStartup) {
	if os.Getenv(guiStartupFD) != "3" {
		return func(guiStartup) {}
	}
	_ = os.Unsetenv(guiStartupFD)
	file := os.NewFile(3, "gui-startup")
	syscall.CloseOnExec(3)
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		_ = file.Close()
		return func(guiStartup) {}
	}
	var once sync.Once
	return func(result guiStartup) {
		once.Do(func() { _ = json.NewEncoder(file).Encode(result); _ = file.Close() })
	}
}

func startBackgroundGUI(ctx context.Context, output io.Writer) error {
	if output == nil {
		output = io.Discard
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	logPath, err := desktopLogPath()
	if err != nil {
		return err
	}
	if err := localfile.EnsurePrivateDir(filepath.Dir(logPath)); err != nil {
		return err
	}
	log, err := localfile.OpenPrivate(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return err
	}
	defer log.Close()
	input, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer input.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()
	command := exec.Command(executable, "gui", "--foreground")
	command.Stdin, command.Stdout, command.Stderr = input, log, log
	command.ExtraFiles = []*os.File{writer}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, guiStartupFD+"=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, guiStartupFD+"=3")
	if err := command.Start(); err != nil {
		return err
	}
	_ = writer.Close()
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	notified := make(chan guiStartup, 1)
	go func() {
		var result guiStartup
		if err := json.NewDecoder(io.LimitReader(reader, 8192)).Decode(&result); err != nil {
			result.Error = "GUI exited before startup completed; see " + logPath
		}
		notified <- result
	}()
	stop := func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			_ = command.Process.Kill()
			<-exited
		}
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case result := <-notified:
		if result.Error != "" {
			stop()
			return errors.New(result.Error)
		}
		if result.Started {
			fmt.Fprintf(output, "GUI running in background (PID %d).\nLog: %s\n", command.Process.Pid, logPath)
		} else {
			// A repeat activation exits immediately and should be reaped before the
			// invoking command returns, without affecting the existing service.
			<-exited
			fmt.Fprintln(output, "Opened the interface in the existing GUI instance.")
		}
		fmt.Fprintln(output, "Stop: pkudisk gui stop")
		return nil
	case <-ctx.Done():
		stop()
		return ctx.Err()
	case <-timer.C:
		stop()
		return fmt.Errorf("GUI startup timed out; see %s", logPath)
	}
}

func desktopLogPath() (string, error) {
	if runtime.GOOS == "linux" {
		if state := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(state) {
			return filepath.Join(state, "pkudisk", "logs", "desktop.log"), nil
		}
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(homeDir, "Library", "Logs", "pkudisk", "desktop.log"), nil
	}
	return filepath.Join(homeDir, ".local", "state", "pkudisk", "logs", "desktop.log"), nil
}
