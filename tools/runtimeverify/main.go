//go:build windows

// runtimeverify starts the built Windows application with isolated state and
// exercises its public HTTP API against a local byte-range fixture server.
package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type check struct {
	Name    string `json:"check"`
	Passed  bool   `json:"passed"`
	Details any    `json:"details,omitempty"`
}

type evidence struct {
	Executable       string    `json:"executable"`
	ExecutableSHA256 string    `json:"executable_sha256"`
	Started          time.Time `json:"started"`
	Finished         time.Time `json:"finished"`
	Passed           bool      `json:"passed"`
	Failure          string    `json:"failure,omitempty"`
	Checks           []check   `json:"checks"`
}

type runner struct {
	exe, root, base, config, logPath string
	apiPort, proxyPort               int
	client                           *http.Client
	process                          *exec.Cmd
	processDone                      chan error
	stdout                           *os.File
	db                               *sql.DB
	proof                            evidence
}

func main() {
	executable := flag.String("exe", "dist/localfix8/wx_video_download.exe", "built application executable")
	output := flag.String("output", "audit/runtime-go", "new isolated output directory")
	flag.Parse()
	r := &runner{client: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 25 * time.Second}}
	err := r.run(*executable, *output)
	r.cleanup()
	r.proof.Finished = time.Now()
	r.proof.Passed = err == nil
	if err != nil {
		r.proof.Failure = err.Error()
	}
	if r.root != "" {
		if writeErr := r.save(); writeErr != nil {
			fmt.Fprintln(os.Stderr, writeErr)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Printf("All %d runtime checks passed. Evidence: %s\n", len(r.proof.Checks), filepath.Join(r.root, "verification.json"))
}

func (r *runner) record(name string, details any) {
	r.proof.Checks = append(r.proof.Checks, check{Name: name, Passed: true, Details: details})
	fmt.Println("PASS", name)
	_ = r.save()
}

func (r *runner) save() error {
	data, err := json.MarshalIndent(r.proof, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.root, "verification.json"), append(data, '\n'), 0600)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitFor(label string, timeout time.Duration, predicate func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		ok, err := predicate()
		if ok && err == nil {
			return nil
		}
		if err != nil {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s timed out: %v", label, last)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func (r *runner) request(path string, payload any, target any) error {
	method := http.MethodGet
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body, method = bytes.NewReader(data), http.MethodPost
	}
	req, err := http.NewRequest(method, r.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024))
	if err != nil {
		return err
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%s HTTP %d: %s", path, response.StatusCode, raw)
	}
	if response.StatusCode != http.StatusOK || envelope.Code != 0 {
		return fmt.Errorf("%s HTTP %d: %s", path, response.StatusCode, raw)
	}
	if target == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, target)
}

func (r *runner) spawn(output io.Writer) (*exec.Cmd, chan error, error) {
	cmd := exec.Command(r.exe, "--config", r.config)
	cmd.Dir = r.root
	cmd.Env = append(os.Environ(), "TMP="+filepath.Join(r.root, "temp"), "TEMP="+filepath.Join(r.root, "temp"))
	cmd.Stdout, cmd.Stderr = output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, done, nil
}

func (r *runner) start() (map[string]any, error) {
	cmd, done, err := r.spawn(r.stdout)
	if err != nil {
		return nil, err
	}
	r.process, r.processDone = cmd, done
	var status map[string]any
	err = waitFor("API and proxy startup", 35*time.Second, func() (bool, error) {
		if err := r.request("/api/status", nil, &status); err != nil {
			return false, err
		}
		for _, key := range []string{"api", "proxy"} {
			service, ok := status[key].(map[string]any)
			if !ok || service["listening"] != true {
				return false, fmt.Errorf("%s not listening: %v", key, status)
			}
		}
		return true, nil
	})
	return status, err
}

func (r *runner) openDB() error {
	if r.db != nil {
		_ = r.db.Close()
	}
	var err error
	r.db, err = sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(r.root, "data.db"))+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return err
	}
	return r.db.Ping()
}

func (r *runner) state(id int) (int, sql.NullInt64, string, error) {
	var state int
	var deleted sql.NullInt64
	var message string
	err := r.db.QueryRow("SELECT status, deleted_at, COALESCE(error_message,'') FROM download_task WHERE id=?", id).Scan(&state, &deleted, &message)
	return state, deleted, message, err
}

func (r *runner) downloaded(id int) (int64, error) {
	var size int64
	err := r.db.QueryRow("SELECT COALESCE(SUM(downloaded),0) FROM download_resource WHERE task_id=?", id).Scan(&size)
	return size, err
}

func (r *runner) create(name, source string, automatic bool) (int, error) {
	var reply struct {
		Tasks []struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
			Data    struct {
				Task struct {
					ID int `json:"id"`
				} `json:"task"`
				Resource struct {
					UniqueID string `json:"unique_id"`
				} `json:"resource"`
			} `json:"data"`
		} `json:"tasks"`
	}
	err := r.request("/api/v1/download_task/create_by_url", map[string]any{"objects": []any{map[string]any{"url": source, "filename": name, "auto_start": automatic}}}, &reply)
	if err != nil {
		return 0, err
	}
	if len(reply.Tasks) != 1 || !reply.Tasks[0].Success || reply.Tasks[0].Data.Task.ID <= 0 || reply.Tasks[0].Data.Resource.UniqueID == "" {
		return 0, fmt.Errorf("non-executable create response: %+v", reply)
	}
	return reply.Tasks[0].Data.Task.ID, nil
}

func (r *runner) action(name string, id int, extra map[string]any) error {
	payload := map[string]any{"task_ids": []int{id}}
	for key, value := range extra {
		payload[key] = value
	}
	var reply struct {
		Results []struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"results"`
	}
	if err := r.request("/api/v1/download_task/"+name, payload, &reply); err != nil {
		return err
	}
	if len(reply.Results) != 1 || !reply.Results[0].Success {
		return fmt.Errorf("%s task %d failed: %+v", name, id, reply)
	}
	return nil
}

func (r *runner) progress(id int) error {
	return waitFor(fmt.Sprintf("task %d progress", id), 25*time.Second, func() (bool, error) {
		size, err := r.downloaded(id)
		return size > 131072, err
	})
}

func (r *runner) finish(id int, expected []byte) (string, error) {
	if err := waitFor(fmt.Sprintf("task %d finish", id), 75*time.Second, func() (bool, error) {
		state, _, message, err := r.state(id)
		if state == 6 {
			return false, fmt.Errorf("task failed: %s", message)
		}
		return state == 5, err
	}); err != nil {
		return "", err
	}
	var directory, name string
	if err := r.db.QueryRow("SELECT download_dir,name FROM download_resource WHERE task_id=? AND deleted_at IS NULL ORDER BY id", id).Scan(&directory, &name); err != nil {
		return "", err
	}
	path := filepath.Join(directory, name)
	actual, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	want, got := sha256.Sum256(expected), sha256.Sum256(actual)
	if got != want {
		return "", fmt.Errorf("task %d SHA256 mismatch: got=%x want=%x length=%d/%d", id, got, want, len(actual), len(expected))
	}
	return path, nil
}

func (r *runner) kill() error {
	if r.process == nil {
		return nil
	}
	_ = r.process.Process.Kill()
	select {
	case <-r.processDone:
		r.process, r.processDone = nil, nil
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("test child did not exit after kill")
	}
}

func (r *runner) cleanup() {
	_ = r.kill()
	if r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
	if r.stdout != nil {
		_ = r.stdout.Close()
		r.stdout = nil
	}
}

func fixtureHandler(data []byte, flaky *atomic.Bool) http.Handler {
	ranges := regexp.MustCompile(`^bytes=(\d+)-(\d*)$`)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/flaky") && !flaky.Load() {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(503)
			return
		}
		size := 1024 * 1024
		slow := strings.Contains(req.URL.Path, "slow")
		if slow {
			size = len(data)
		}
		start, end := 0, size-1
		status := http.StatusOK
		if raw := req.Header.Get("Range"); raw != "" {
			found := ranges.FindStringSubmatch(raw)
			if found == nil {
				w.WriteHeader(416)
				return
			}
			start, _ = strconv.Atoi(found[1])
			if found[2] != "" {
				requested, _ := strconv.Atoi(found[2])
				if requested < end {
					end = requested
				}
			}
			if start > end || start < 0 {
				w.WriteHeader(416)
				return
			}
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"runtime-go-fixture-v1"`)
		w.WriteHeader(status)
		if req.Method == http.MethodHead {
			return
		}
		for offset := start; offset <= end; offset += 65536 {
			next := offset + 65536
			if next > end+1 {
				next = end + 1
			}
			if _, err := w.Write(data[offset:next]); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			if slow {
				select {
				case <-req.Context().Done():
					return
				case <-time.After(40 * time.Millisecond):
				}
			}
		}
	})
}

func (r *runner) run(executable, output string) error {
	var err error
	r.exe, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	r.root, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	if _, err := os.Stat(r.root); err == nil {
		r.root = ""
		return errors.New("output directory already exists; choose a new one to preserve evidence")
	}
	if err := os.MkdirAll(filepath.Join(r.root, "downloads"), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.root, "temp"), 0700); err != nil {
		return err
	}
	exeBytes, err := os.ReadFile(r.exe)
	if err != nil {
		return err
	}
	exeHash := sha256.Sum256(exeBytes)
	r.proof = evidence{Executable: r.exe, ExecutableSHA256: hex.EncodeToString(exeHash[:]), Started: time.Now(), Checks: []check{}}
	r.apiPort, err = freePort()
	if err != nil {
		return err
	}
	for r.proxyPort == 0 || r.proxyPort == r.apiPort {
		r.proxyPort, err = freePort()
		if err != nil {
			return err
		}
	}
	r.base = fmt.Sprintf("http://127.0.0.1:%d", r.apiPort)
	r.config = filepath.Join(r.root, "config.yaml")
	r.logPath = filepath.Join(r.root, "temp", "wx_channels_download", "app.log")
	quote := func(value string) string { data, _ := json.Marshal(value); return string(data) }
	config := fmt.Sprintf("workdir: %s\napi:\n  hostname: 127.0.0.1\n  port: %d\nproxy:\n  enabled: true\n  system: false\n  tun: false\n  skipInstallRootCert: true\n  hostname: 127.0.0.1\n  port: %d\ndownload:\n  dir: %s\n  filenameTemplate: \"{{filename}}\"\n  playDoneAudio: false\n  maxRunning: 1\n  resourceConcurrency: 2\n  segmentConcurrency: 2\n  connectionConcurrency: 4\n  speedLimitMBps: 4\ndb:\n  filepath: %s\nmcp:\n  enabled: true\nbridge:\n  enabled: false\n", quote(r.root), r.apiPort, r.proxyPort, quote(filepath.Join(r.root, "downloads")), quote(filepath.Join(r.root, "data.db")))
	if err := os.WriteFile(r.config, []byte(config), 0600); err != nil {
		return err
	}
	r.stdout, err = os.OpenFile(filepath.Join(r.root, "stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	payload := make([]byte, 32*1024*1024)
	for index := range payload {
		payload[index] = byte(index)
	}
	var flaky atomic.Bool
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	fixture := &http.Server{Handler: fixtureHandler(payload, &flaky)}
	go func() { _ = fixture.Serve(listener) }()
	defer fixture.Close()
	fixtureURL := "http://" + listener.Addr().String()
	status, err := r.start()
	if err != nil {
		return err
	}
	if err := r.openDB(); err != nil {
		return err
	}
	r.record("binary starts with isolated API and proxy", status)
	if err := r.checkAssetsAndProxy(fixtureURL, payload[:1024*1024]); err != nil {
		return err
	}
	id, err := r.create("multi\nline\tfixture.bin", fixtureURL+"/normal.bin", false)
	if err != nil {
		return err
	}
	state, _, _, err := r.state(id)
	if err != nil {
		return err
	}
	if state != 0 {
		return fmt.Errorf("manual task starts in state %d", state)
	}
	if err := r.action("start", id, nil); err != nil {
		return err
	}
	path, err := r.finish(id, payload[:1024*1024])
	if err != nil {
		return err
	}
	if strings.ContainsAny(filepath.Base(path), "\r\n\t") {
		return fmt.Errorf("unsafe filename: %q", path)
	}
	r.record("create, start, multiline filename and SHA256", map[string]any{"task_id": id, "path": path, "bytes": 1024 * 1024})
	id, err = r.create("pause-resume.bin", fixtureURL+"/slow-pause.bin", true)
	if err != nil {
		return err
	}
	if err := r.progress(id); err != nil {
		return err
	}
	if err := r.checkDuplicateInstance(id); err != nil {
		return err
	}
	if err := r.action("pause", id, nil); err != nil {
		return err
	}
	state, _, _, err = r.state(id)
	if err != nil {
		return err
	}
	if state != 3 {
		return fmt.Errorf("pause state=%d", state)
	}
	paused, err := r.downloaded(id)
	if err != nil {
		return err
	}
	time.Sleep(600 * time.Millisecond)
	stable, err := r.downloaded(id)
	if err != nil {
		return err
	}
	if stable != paused {
		return fmt.Errorf("paused bytes grew: %d to %d", paused, stable)
	}
	if err := r.action("resume", id, nil); err != nil {
		return err
	}
	path, err = r.finish(id, payload)
	if err != nil {
		return err
	}
	r.record("32 MiB pause/resume preserves exact bytes", map[string]any{"task_id": id, "paused_bytes": paused, "path": path, "sha256": fmt.Sprintf("%x", sha256.Sum256(payload))})
	if err := r.checkDelete(fixtureURL); err != nil {
		return err
	}
	id, err = r.create("retry.bin", fixtureURL+"/flaky.bin", true)
	if err != nil {
		return err
	}
	if err := waitFor("failed HTTP task state", 40*time.Second, func() (bool, error) { state, _, _, err := r.state(id); return state == 6, err }); err != nil {
		return err
	}
	flaky.Store(true)
	if err := r.action("retry", id, nil); err != nil {
		return err
	}
	if _, err := r.finish(id, payload[:1024*1024]); err != nil {
		return err
	}
	r.record("failed HTTP task retries successfully", map[string]any{"task_id": id})
	id, err = r.create("crash-recover.bin", fixtureURL+"/slow-crash.bin", true)
	if err != nil {
		return err
	}
	if err := r.progress(id); err != nil {
		return err
	}
	before, err := r.downloaded(id)
	if err != nil {
		return err
	}
	if err := r.kill(); err != nil {
		return err
	}
	priorLog, err := os.ReadFile(r.logPath)
	if err != nil {
		return err
	}
	if _, err := r.start(); err != nil {
		return err
	}
	if err := r.openDB(); err != nil {
		return err
	}
	state, _, message, err := r.state(id)
	if err != nil {
		return err
	}
	if state != 3 {
		return fmt.Errorf("crash task recovery state=%d message=%s", state, message)
	}
	after, err := r.downloaded(id)
	if err != nil {
		return err
	}
	if after < before {
		return fmt.Errorf("crash lost persisted progress: %d -> %d", before, after)
	}
	currentLog, err := os.ReadFile(r.logPath)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(currentLog, priorLog) {
		return errors.New("restart truncated previous application log")
	}
	if err := r.action("resume", id, nil); err != nil {
		return err
	}
	path, err = r.finish(id, payload)
	if err != nil {
		return err
	}
	r.record("forced exit, startup recovery, 32 MiB resume and log preservation", map[string]any{"task_id": id, "before_kill_bytes": before, "recovered_bytes": after, "path": path, "sha256": fmt.Sprintf("%x", sha256.Sum256(payload))})
	if err := r.request("/api/service/stop?name=application", map[string]any{}, nil); err != nil {
		return err
	}
	select {
	case err := <-r.processDone:
		r.process, r.processDone = nil, nil
		if err != nil {
			return fmt.Errorf("graceful exit: %w", err)
		}
	case <-time.After(20 * time.Second):
		return errors.New("graceful stop timed out")
	}
	r.record("graceful application stop", nil)
	var integrity string
	if err := r.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("database integrity: %s", integrity)
	}
	r.record("test database integrity", integrity)
	return nil
}

func (r *runner) checkAssetsAndProxy(fixtureURL string, expected []byte) error {
	response, err := r.client.Get(r.base + "/")
	if err != nil {
		return err
	}
	html, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		return fmt.Errorf("served UI HTTP %d", response.StatusCode)
	}
	if err := os.WriteFile(filepath.Join(r.root, "served-index.html"), html, 0600); err != nil {
		return err
	}
	assets := regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllSubmatch(html, -1)
	base, _ := url.Parse(r.base + "/")
	seen := map[string]bool{}
	for _, match := range assets {
		path := string(match[1])
		if !(strings.HasPrefix(path, "src/") || strings.HasPrefix(path, "public/") || strings.HasPrefix(path, "/__assets/") || strings.HasPrefix(path, "/favicon.ico")) {
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		relative, err := url.Parse(path)
		if err != nil {
			return err
		}
		response, err := r.client.Get(base.ResolveReference(relative).String())
		if err != nil {
			return err
		}
		content, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return err
		}
		if response.StatusCode != 200 || len(content) == 0 {
			return fmt.Errorf("UI asset failed: %s status=%d bytes=%d", path, response.StatusCode, len(content))
		}
	}
	if len(seen) == 0 {
		return errors.New("UI has no embedded referenced assets")
	}
	r.record("served UI and referenced assets", map[string]int{"asset_count": len(seen)})
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", r.proxyPort))
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	response, err = client.Get(fixtureURL + "/proxy-proof.bin")
	if err != nil {
		return err
	}
	actual, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return err
	}
	if response.StatusCode != 200 || !bytes.Equal(actual, expected) {
		return fmt.Errorf("HTTP proxy returned status=%d bytes=%d", response.StatusCode, len(actual))
	}
	r.record("actual HTTP proxy forwards exact bytes", nil)
	return nil
}

func (r *runner) checkDuplicateInstance(activeID int) error {
	path := filepath.Join(r.root, "competing-instance.log")
	output, err := os.Create(path)
	if err != nil {
		return err
	}
	cmd, done, err := r.spawn(output)
	if err != nil {
		output.Close()
		return err
	}
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		output.Close()
		return errors.New("duplicate instance did not reject startup promptly")
	}
	output.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "already using this database") {
		return fmt.Errorf("duplicate process missing lock rejection: %s", data)
	}
	state, _, _, err := r.state(activeID)
	if err != nil {
		return err
	}
	if state != 2 {
		return fmt.Errorf("duplicate process changed active state to %d", state)
	}
	r.record("duplicate instance blocked before changing active task state", map[string]any{"active_task": activeID, "status": state})
	return nil
}

func (r *runner) checkDelete(fixtureURL string) error {
	id, err := r.create("cancel-active.bin", fixtureURL+"/slow-cancel.bin", true)
	if err != nil {
		return err
	}
	if err := r.progress(id); err != nil {
		return err
	}
	var directory, name, unique string
	if err := r.db.QueryRow("SELECT download_dir,name,unique_id FROM download_resource WHERE task_id=?", id).Scan(&directory, &name, &unique); err != nil {
		return err
	}
	if err := r.action("delete", id, map[string]any{"delete_files": true}); err != nil {
		return err
	}
	_, deleted, _, err := r.state(id)
	if err != nil {
		return err
	}
	if !deleted.Valid {
		return errors.New("active deletion did not mark task deleted")
	}
	for _, base := range []string{name, unique} {
		for _, suffix := range []string{"", ".part", ".tmp", ".tmp.part"} {
			path := filepath.Join(directory, base+suffix)
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("active deletion left file %s", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	var liveResources int
	if err := r.db.QueryRow("SELECT count(*) FROM download_resource WHERE task_id=? AND deleted_at IS NULL", id).Scan(&liveResources); err != nil {
		return err
	}
	if liveResources != 0 {
		return fmt.Errorf("deleted task has %d active resource records", liveResources)
	}
	r.record("delete running task, database graph and fixture partial file", map[string]any{"task_id": id})
	return nil
}
