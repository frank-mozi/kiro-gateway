package portable

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type child struct {
	cmd      *exec.Cmd
	input    io.WriteCloser
	done     chan struct{}
	err      error
	instance string
}

type pointer struct {
	Directory string `json:"directory"`
}
type transaction struct {
	Previous string `json:"previous"`
	Backup   string `json:"backup"`
	Failed   string `json:"failed"`
}

type Manager struct {
	root      string
	port      int
	token     string
	address   string
	log       *os.File
	http      *http.Client
	ctx       context.Context
	mu        sync.Mutex
	operation sync.Mutex
	state     State
	current   string
	child     *child
}

// Run keeps the launcher alive during upgrades. Closing it also stops its child.
func Run(ctx context.Context, root string, port int, launcherVersion string, ready func(string)) error {
	if port < 1024 || port > 65535 {
		return errors.New("portable port must be between 1024 and 65535")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if root == filepath.VolumeName(root)+string(os.PathSeparator) {
		return errors.New("the filesystem root is not a portable installation directory")
	}
	lock, err := os.OpenFile(filepath.Join(root, ".portable.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return errors.New("this portable directory already has a running launcher")
	}
	probe, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("gateway port is unavailable: %w", err)
	}
	probe.Close()
	for _, dir := range []string{"data", "releases", "downloads", "backups", "logs"} {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("portable directory must not be a link: %s", dir)
		}
	}
	log, err := os.OpenFile(filepath.Join(root, "logs", "gateway.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	m := &Manager{root: root, port: port, log: log, ctx: ctx, token: randomID(),
		http:  &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 65 * time.Second},
		state: State{Managed: true, Root: root, Launcher: launcherVersion, Phase: "starting"}}
	if err := m.recoverTransaction(); err != nil {
		return fmt.Errorf("recover interrupted upgrade: %w", err)
	}
	m.current, err = m.readPointer()
	if err != nil {
		return err
	}
	raw, err := readLimited(filepath.Join(root, "update-source.json"), 16384)
	if err == nil {
		if err := json.Unmarshal(raw, &m.state.Source); err != nil {
			return fmt.Errorf("read update source: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.state.Source, err = storedSource(m.state.Source)
	if err != nil {
		return fmt.Errorf("read online update source: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	m.address = "http://" + ln.Addr().String()
	server := &http.Server{Handler: m, ReadHeaderTimeout: 5 * time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(ln) }()
	defer server.Close()
	defer func() {
		if m.child != nil {
			if err := m.stop(); err != nil {
				fmt.Fprintln(log, "portable shutdown:", err)
			}
		}
	}()
	if err := func() error {
		m.operation.Lock()
		defer m.operation.Unlock()
		if err := m.start(m.current, false); err != nil {
			return err
		}
		if err := m.waitHealthy(""); err != nil {
			return err
		}
		m.phase("idle", nil)
		return nil
	}(); err != nil {
		return err
	}
	if ready != nil {
		ready(fmt.Sprintf("http://127.0.0.1:%d", port))
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Let an already-started transaction finish or roll back before exit.
			m.operation.Lock()
			defer m.operation.Unlock()
			return nil
		case err := <-serverDone:
			m.operation.Lock()
			defer m.operation.Unlock()
			return fmt.Errorf("portable control server: %w", err)
		case <-ticker.C:
			if m.operation.TryLock() {
				if m.child != nil {
					select {
					case <-m.child.done:
						err := m.child.err
						m.child = nil
						m.operation.Unlock()
						return fmt.Errorf("gateway exited unexpectedly: %v; inspect logs/gateway.log", err)
					default:
					}
				}
				m.operation.Unlock()
			}
		}
	}
}

func randomID() string {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data[:])
}

func (m *Manager) snapshot() State { m.mu.Lock(); defer m.mu.Unlock(); return m.state }
func (m *Manager) phase(phase string, err error) {
	m.mu.Lock()
	m.state.Phase = phase
	m.state.Error = ""
	if err != nil {
		m.state.Error = err.Error()
	}
	m.mu.Unlock()
	fmt.Fprintf(m.log, "%s portable: %s", time.Now().Format(time.RFC3339), phase)
	if err != nil {
		fmt.Fprintf(m.log, ": %v", err)
	}
	fmt.Fprintln(m.log)
}

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !authorized(r, m.token) {
		writeFailure(w, 401, "unauthorized")
		return
	}
	if m.ctx.Err() != nil {
		writeFailure(w, 503, "portable launcher is stopping")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/status" {
		writeResponse(w, m.snapshot())
		return
	}
	if r.Method != http.MethodPost {
		writeFailure(w, 405, "method not allowed")
		return
	}
	if !m.operation.TryLock() {
		writeFailure(w, 409, "another portable operation is in progress")
		return
	}
	async := false
	defer func() {
		if !async {
			m.operation.Unlock()
		}
	}()
	switch r.URL.Path {
	case "/source":
		var source Source
		if !decodeBody(w, r, &source) {
			return
		}
		source, err := onlineSource(source)
		if err != nil {
			writeFailure(w, 400, err.Error())
			return
		}
		if err := writeJSONFile(filepath.Join(m.root, "update-source.json"), source); err != nil {
			writeFailure(w, 500, err.Error())
			return
		}
		m.mu.Lock()
		m.state.Source, m.state.Latest, m.state.Notes, m.state.CheckedAt, m.state.UpdateAvailable = source, "", "", "", false
		m.mu.Unlock()
		m.phase("idle", nil)
	case "/check":
		if _, err := m.check(); err != nil {
			m.phase("check_failed", err)
			writeFailure(w, 400, err.Error())
			return
		}
		m.phase("checked", nil)
	case "/install":
		var body struct {
			Version string `json:"version"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		state := m.snapshot()
		if !state.UpdateAvailable || state.Latest != body.Version {
			writeFailure(w, 409, "check for updates and confirm the displayed version first")
			return
		}
		async = true
		m.phase("verifying", nil)
		go func() {
			defer m.operation.Unlock()
			if err := m.install(body.Version); err != nil {
				if m.snapshot().Phase != "rolled_back" {
					m.phase("failed", err)
				}
			}
		}()
	default:
		writeFailure(w, 404, "unknown portable operation")
		return
	}
	writeResponse(w, m.snapshot())
}

func (m *Manager) check() (Release, error) {
	state := m.snapshot()
	// Failed fresh checks must not leave a stale "update available" result.
	m.mu.Lock()
	m.state.UpdateAvailable = false
	m.state.CheckedAt = ""
	m.state.Latest = ""
	m.state.Notes = ""
	m.mu.Unlock()
	release, err := onlineRelease(m.ctx, releaseHTTP, state.Source)
	if err != nil {
		return release, err
	}
	if _, err := release.asset(); err != nil {
		return release, err
	}
	if !stableVersion.MatchString(strings.TrimPrefix(state.Current, "local-")) {
		return release, errors.New("running build has no comparable release version")
	}
	m.mu.Lock()
	m.state.Latest, m.state.Notes = release.Version, release.Notes
	m.state.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	m.state.UpdateAvailable = newer(state.Current, release.Version)
	m.mu.Unlock()
	return release, nil
}

func (m *Manager) readPointer() (string, error) {
	raw, err := readLimited(filepath.Join(m.root, "current.json"), 4096)
	if errors.Is(err, os.ErrNotExist) {
		return ".", nil
	}
	if err != nil {
		return "", err
	}
	var p pointer
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	if err := validPointer(p.Directory); err != nil {
		return "", err
	}
	return p.Directory, nil
}

func validPointer(path string) error {
	if path == "." {
		return nil
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) != 2 || parts[0] != "releases" || !stableVersion.MatchString(parts[1]) {
		return errors.New("invalid portable release pointer")
	}
	return nil
}

func (m *Manager) start(directory string, hold bool) error {
	if err := validPointer(directory); err != nil {
		return err
	}
	base := filepath.Join(m.root, directory)
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("release directory is missing or is a link")
	}
	if info, err := os.Stat(filepath.Join(base, "frontend", "dist", "index.html")); err != nil || !info.Mode().IsRegular() {
		return errors.New("frontend/dist/index.html is missing; extract the complete portable bundle")
	}
	binary := feedBinaryName(runtime.GOOS)
	if directory == "." {
		binary = BinaryName(runtime.GOOS)
	}
	cmd := exec.Command(filepath.Join(base, binary), "start", "--no-browser", "--managed")
	cmd.Dir = base
	instance := randomID()
	cmd.Env = privateEnvironment(map[string]string{
		"KEIROUTER_PORTABLE_CONTROL": m.address, "KEIROUTER_PORTABLE_TOKEN": m.token,
		"KEIROUTER_PORTABLE_INSTANCE": instance, "KEIROUTER_PORTABLE_HOLD": fmt.Sprint(hold),
		"KEIROUTER_DATA__DIR":    filepath.Join(m.root, "data"),
		"KEIROUTER_FRONTEND_DIR": filepath.Join(base, "frontend", "dist"),
		"KEIROUTER_SERVER__HOST": "127.0.0.1", "KEIROUTER_SERVER__PORT": fmt.Sprint(m.port),
		"KEIROUTER_DATABASE__DRIVER": "sqlite", "KEIROUTER_DATABASE__DSN": "",
		"KEIROUTER_SECURITY__BIND_LOOPBACK_ONLY": "true", "KEIROUTER_SECURITY__MASTER_KEY": "",
	})
	cmd.Stdout, cmd.Stderr = m.log, m.log
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		input.Close()
		return err
	}
	p := &child{cmd: cmd, input: input, done: make(chan struct{}), instance: instance}
	m.child = p
	go func() { p.err = cmd.Wait(); close(p.done) }()
	return nil
}

func (m *Manager) stop() error {
	p := m.child
	if p == nil {
		return nil
	}
	// EOF on the managed stdin pipe asks the child to shut down gracefully.
	p.input.Close()
	select {
	case <-p.done:
		m.child = nil
		return p.err
	case <-time.After(45 * time.Second):
		return errors.New("gateway did not exit; update aborted without copying its live database")
	}
}

func (m *Manager) request(method, path string, value any) error {
	timeout := 3 * time.Second
	if path == "/_portable/drain" {
		timeout = 65 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", m.port, path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("gateway control %s returned HTTP %d", path, resp.StatusCode)
	}
	if value != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(value)
	}
	return nil
}

func (m *Manager) waitHealthy(expected string) error {
	until := time.Now().Add(25 * time.Second)
	for time.Now().Before(until) {
		if m.child == nil {
			return errors.New("no gateway process")
		}
		select {
		case <-m.child.done:
			return fmt.Errorf("gateway exited before readiness: %v; inspect logs/gateway.log", m.child.err)
		default:
		}
		var health struct {
			Version  string `json:"version"`
			Instance string `json:"instance"`
		}
		if err := m.request(http.MethodGet, "/_portable/health", &health); err == nil {
			if health.Instance != m.child.instance {
				return errors.New("readiness response belongs to a different process")
			}
			if expected != "" && health.Version != expected {
				return fmt.Errorf("expected version %s, got %s", expected, health.Version)
			}
			if err := m.request(http.MethodGet, "/", nil); err != nil {
				return fmt.Errorf("dashboard readiness: %w", err)
			}
			m.mu.Lock()
			m.state.Current = health.Version
			m.mu.Unlock()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("gateway readiness timed out; inspect logs/gateway.log")
}

func (m *Manager) install(wanted string) error {
	release, err := m.check()
	if err != nil {
		return err
	}
	if release.Version != wanted || !m.snapshot().UpdateAvailable {
		return errors.New("release changed or is no longer newer; check again")
	}
	m.phase("extracting", nil)
	target, err := stageOnline(m.ctx, releaseHTTP, m.root, m.snapshot().Source, release)
	if err != nil {
		return err
	}
	defer func() {
		_, journalErr := os.Stat(filepath.Join(m.root, "transaction.json"))
		if errors.Is(journalErr, os.ErrNotExist) && m.current != target && (m.child == nil || m.child.cmd.Path != filepath.Join(m.root, target, feedBinaryName(runtime.GOOS))) {
			if err := os.RemoveAll(filepath.Join(m.root, target)); err != nil {
				fmt.Fprintln(m.log, "cannot clean unused staged release:", err)
			}
		}
	}()
	old := m.current
	oldVersion := m.snapshot().Current
	m.phase("draining", nil)
	if err := m.request(http.MethodPost, "/_portable/drain", nil); err != nil {
		resumeErr := m.request(http.MethodPost, "/_portable/resume", nil)
		return errors.Join(err, resumeErr)
	}
	m.phase("stopping", nil)
	if err := m.stop(); err != nil {
		return err
	}
	m.phase("backing_up", nil)
	id := time.Now().UTC().Format("20060102T150405Z") + "-" + randomID()[:8]
	tx := transaction{Previous: old, Backup: filepath.Join("backups", id, "data"), Failed: filepath.Join("backups", id, "failed-data")}
	if err := copyTree(filepath.Join(m.root, "data"), filepath.Join(m.root, tx.Backup)); err != nil {
		return m.restartPrevious(old, oldVersion, fmt.Errorf("backup failed: %w", err))
	}
	if err := writeJSONFile(filepath.Join(m.root, "transaction.json"), tx); err != nil {
		return m.restartPrevious(old, oldVersion, err)
	}
	if err := writeJSONFile(filepath.Join(m.root, "current.json"), pointer{target}); err != nil {
		return m.rollback(tx, oldVersion, err)
	}
	m.phase("restarting", nil)
	if err := m.start(target, true); err != nil {
		return m.rollback(tx, oldVersion, err)
	}
	if err := m.waitHealthy(release.Version); err != nil {
		return m.rollback(tx, oldVersion, err)
	}
	if err := os.Remove(filepath.Join(m.root, "transaction.json")); err != nil {
		return m.rollback(tx, oldVersion, err)
	}
	m.current = target
	if err := m.request(http.MethodPost, "/_portable/resume", nil); err != nil {
		return fmt.Errorf("new version is committed but API resume failed; restart the portable launcher: %w", err)
	}
	m.mu.Lock()
	m.state.UpdateAvailable = false
	m.mu.Unlock()
	m.phase("completed", nil)
	return nil
}

func (m *Manager) restartPrevious(directory, version string, cause error) error {
	if err := m.start(directory, false); err != nil {
		return errors.Join(cause, fmt.Errorf("restart previous version: %w", err))
	}
	if err := m.waitHealthy(version); err != nil {
		return errors.Join(cause, err)
	}
	m.phase("rolled_back", cause)
	return cause
}

func (m *Manager) rollback(tx transaction, version string, cause error) error {
	m.phase("rolling_back", cause)
	if m.child != nil {
		if err := m.stop(); err != nil && m.child != nil {
			return errors.Join(cause, err)
		}
	}
	if err := m.restore(tx); err != nil {
		return errors.Join(cause, fmt.Errorf("restore failed: %w", err))
	}
	m.current = tx.Previous
	return m.restartPrevious(tx.Previous, version, cause)
}

func (m *Manager) recoverTransaction() error {
	raw, err := readLimited(filepath.Join(m.root, "transaction.json"), 16384)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var tx transaction
	if err := json.Unmarshal(raw, &tx); err != nil {
		return err
	}
	return m.restore(tx)
}

func (m *Manager) restore(tx transaction) error {
	if err := validPointer(tx.Previous); err != nil {
		return err
	}
	for _, path := range []string{tx.Backup, tx.Failed} {
		parts := strings.Split(filepath.ToSlash(path), "/")
		if len(parts) != 3 || parts[0] != "backups" || !safeName(parts[1]) || (parts[2] != "data" && parts[2] != "failed-data") {
			return errors.New("invalid recovery path")
		}
	}
	if filepath.Dir(tx.Backup) != filepath.Dir(tx.Failed) || filepath.Base(tx.Backup) != "data" || filepath.Base(tx.Failed) != "failed-data" {
		return errors.New("invalid recovery backup pair")
	}
	if _, err := os.Stat(filepath.Join(m.root, tx.Backup)); err != nil {
		return err
	}
	data := filepath.Join(m.root, "data")
	// Preserve the failed data, including any schema migration, for diagnosis.
	if _, err := os.Stat(data); err == nil {
		failed := filepath.Join(m.root, tx.Failed)
		if _, err := os.Stat(failed); err == nil {
			failed += "-recovery-" + randomID()[:8]
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(data, failed); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staged := filepath.Join(m.root, "downloads", "restore-"+randomID())
	if err := copyTree(filepath.Join(m.root, tx.Backup), staged); err != nil {
		return err
	}
	if err := os.Rename(staged, data); err != nil {
		return fmt.Errorf("activate restored data (preserved at %s): %w", staged, err)
	}
	if err := writeJSONFile(filepath.Join(m.root, "current.json"), pointer{tx.Previous}); err != nil {
		return err
	}
	return os.Remove(filepath.Join(m.root, "transaction.json"))
}

func copyTree(source, target string) error {
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return errors.New("backup/restore destination already exists")
	}
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, relative)
		if info.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		if !info.Mode().IsRegular() {
			return errors.New("data backup refuses links or special files")
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		syncErr := out.Sync()
		return errors.Join(copyErr, syncErr, in.Close(), out.Close())
	})
}
