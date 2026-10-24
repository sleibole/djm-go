package scripts

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("python3"); err != nil {
		fmt.Fprintln(os.Stderr, "python3 is required for dev-server tests")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Dir(filepath.Dir(file))
}

func scriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "scripts", "dev-server.sh")
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testEnv(t *testing.T, extra map[string]string) []string {
	t.Helper()
	vals := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.HasPrefix(k, "DEV_SERVER_") {
			continue
		}
		vals[k] = v
	}
	for k, v := range extra {
		vals[k] = v
	}
	env := make([]string, 0, len(vals))
	for k, v := range vals {
		env = append(env, k+"="+v)
	}
	return env
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func httpServer(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "server.py")
	writeFile(t, path, `#!/usr/bin/env python3
import os, signal, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Leave the parent's process group, the way Air places the Go binary.
os.setpgrp()

port = int(os.environ["PORT"])
out = os.environ.get("DEV_SERVER_ENV_OUT", "")
if out:
    with open(out, "w", encoding="utf-8") as fh:
        for key in ("ADDR", "PORT", "DB_PATH", "BASE_URL", "BLOB_DIR", "CATALOG_CACHE_DIR"):
            fh.write(os.environ.get(key, "") + "\n")

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"ok\n"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        return

def stop(signum, frame):
    sys.exit(0)

signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
signal.signal(signal.SIGHUP, stop)
ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
`, 0o755)
	return path
}

func shellCommand(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "cmd.sh")
	writeFile(t, path, "#!/bin/sh\n"+body, 0o755)
	return path
}

type agent struct {
	name   string
	cmd    *exec.Cmd
	out    *safeBuf
	exited chan struct{}
	err    error
}

func startAgent(t *testing.T, env []string, name string, setpgid bool) *agent {
	t.Helper()
	a := &agent{
		name:   name,
		out:    &safeBuf{},
		exited: make(chan struct{}),
		cmd:    exec.Command(scriptPath(t), "start", name),
	}
	a.cmd.Env = env
	a.cmd.Stdout = a.out
	a.cmd.Stderr = a.out
	if setpgid {
		a.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := a.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		a.err = a.cmd.Wait()
		close(a.exited)
	}()
	t.Cleanup(func() {
		stopName(env, name)
		select {
		case <-a.exited:
		case <-time.After(8 * time.Second):
			_ = a.cmd.Process.Kill()
			<-a.exited
		}
	})
	return a
}

func (a *agent) ready(t *testing.T) {
	t.Helper()
	needle := "dev-agent: ready name=" + a.name + " "
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(a.out.String(), needle) {
			return
		}
		select {
		case <-a.exited:
			t.Fatalf("dev-agent %s exited before ready: %v\n%s", a.name, a.err, a.out.String())
		default:
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("dev-agent %s was not ready\n%s", a.name, a.out.String())
}

func (a *agent) waitExit(t *testing.T) {
	t.Helper()
	select {
	case <-a.exited:
	case <-time.After(8 * time.Second):
		t.Fatalf("dev-agent %s did not exit\n%s", a.name, a.out.String())
	}
}

func stopName(env []string, name string) {
	cmd := exec.Command(scriptFromEnv(env), "stop", name)
	cmd.Env = env
	_ = cmd.Run()
}

func scriptFromEnv(env []string) string {
	// Tests always invoke the script by absolute path via scriptPath.
	// stopName is also used from Cleanup, which cannot call t.Helper after fail.
	for _, kv := range env {
		if strings.HasPrefix(kv, "DEV_SERVER_SCRIPT=") {
			return strings.TrimPrefix(kv, "DEV_SERVER_SCRIPT=")
		}
	}
	return ""
}

func baseExtra(t *testing.T, state string, min, max int, command string) map[string]string {
	t.Helper()
	return map[string]string{
		"DEV_SERVER_STATE_DIR":     state,
		"DEV_SERVER_PORT_MIN":      strconv.Itoa(min),
		"DEV_SERVER_PORT_MAX":      strconv.Itoa(max),
		"DEV_SERVER_READY_TIMEOUT": "10",
		"DEV_SERVER_COMMAND":       command,
		"DEV_SERVER_SCRIPT":        scriptPath(t),
	}
}

func runScript(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(scriptPath(t), args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func instanceField(t *testing.T, state, name, key string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(state, name, "instance.env"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := key + "="
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("instance %s has no %s\n%s", name, key, body)
	return ""
}

func waitInstance(t *testing.T, state, name, key string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(filepath.Join(state, name, "instance.env"))
		if err == nil {
			prefix := key + "="
			for _, line := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(line, prefix) {
					value := strings.TrimPrefix(line, prefix)
					if value != "" {
						return value
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s %s", name, key)
	return ""
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("pid %d is still running", pid)
}

func assertNotPersonal(t *testing.T, paths ...string) {
	t.Helper()
	root := repoRoot(t)
	personal := filepath.Join(root, "data") + string(os.PathSeparator)
	db := filepath.Join(root, "data", "djm.db")
	blobs := filepath.Join(root, "data", "blobs")
	catalogs := filepath.Join(root, "data", "catalogs")
	for _, path := range paths {
		if path == db || path == blobs || path == catalogs || strings.HasPrefix(path, personal) {
			t.Errorf("path uses personal data: %s", path)
		}
	}
}

func dialOK(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

// Test ports are 39100 and above. track-anything's dev-server tests use 38100 and above,
// so the two suites can run at the same time without taking each other's ports.
func TestStartStopRestartAndIsolation(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	envOut := filepath.Join(dir, "env.txt")
	extra := baseExtra(t, state, 39100, 39109, httpServer(t, dir))
	extra["DEV_SERVER_ENV_OUT"] = envOut
	env := testEnv(t, extra)

	first := startAgent(t, env, "alpha", false)
	first.ready(t)
	port := instanceField(t, state, "alpha", "port")
	if port != "39100" {
		t.Fatalf("port = %s, want 39100", port)
	}
	got, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 6 {
		t.Fatalf("env lines = %q", lines)
	}
	if lines[0] != ":"+port || lines[1] != port {
		t.Errorf("ADDR/PORT = %q %q", lines[0], lines[1])
	}
	wantDB := filepath.Join(state, "alpha", "data", "djm.db")
	wantBlobs := filepath.Join(state, "alpha", "blobs")
	wantCatalogs := filepath.Join(state, "alpha", "catalogs")
	if lines[2] != wantDB || lines[4] != wantBlobs || lines[5] != wantCatalogs {
		t.Errorf("paths = %q, want %s %s %s", lines, wantDB, wantBlobs, wantCatalogs)
	}
	if lines[3] != "http://localhost:"+port {
		t.Errorf("BASE_URL = %q", lines[3])
	}
	assertNotPersonal(t, lines[2], lines[4], lines[5])
	for _, dir := range []string{filepath.Join(state, "alpha", "data"), wantBlobs, wantCatalogs, filepath.Join(state, "alpha", "air")} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("missing instance dir %s: %v", dir, err)
		}
	}
	dialOK(t, "127.0.0.1:"+port)

	list, err := runScript(t, env, "list")
	if err != nil {
		t.Fatal(err, list)
	}
	if !strings.Contains(list, "alpha "+port+" http://localhost:"+port+" running") {
		t.Fatalf("list = %q", list)
	}

	again, againErr := runScript(t, env, "start", "alpha")
	if againErr != nil {
		t.Fatalf("second start: %v\n%s", againErr, again)
	}
	if !strings.Contains(again, "already running at http://localhost:"+port) {
		t.Fatalf("second start = %q", again)
	}

	child, _ := strconv.Atoi(instanceField(t, state, "alpha", "go_pid"))
	if _, err := runScript(t, env, "stop", "alpha"); err != nil {
		t.Fatal(err)
	}
	first.waitExit(t)
	waitDead(t, child)
	list, err = runScript(t, env, "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(list) != "no agent dev servers" {
		t.Fatalf("list after stop = %q", list)
	}

	second := startAgent(t, env, "alpha", false)
	second.ready(t)
	if got := instanceField(t, state, "alpha", "port"); got != port {
		t.Fatalf("restart port = %s, want %s", got, port)
	}
}

func TestStopOneLeavesTheOther(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	env := testEnv(t, baseExtra(t, state, 39110, 39119, httpServer(t, dir)))
	a := startAgent(t, env, "alpha", false)
	b := startAgent(t, env, "beta", false)
	a.ready(t)
	b.ready(t)
	aPort := instanceField(t, state, "alpha", "port")
	bPort := instanceField(t, state, "beta", "port")
	if aPort == bPort {
		t.Fatalf("both used port %s", aPort)
	}
	aBin := instanceField(t, state, "alpha", "binary")
	bBin := instanceField(t, state, "beta", "binary")
	if aBin == bBin || !strings.Contains(aBin, "alpha") || !strings.Contains(bBin, "beta") {
		t.Fatalf("binaries = %s and %s", aBin, bBin)
	}
	aPID, _ := strconv.Atoi(instanceField(t, state, "alpha", "go_pid"))
	if _, err := runScript(t, env, "stop", "alpha"); err != nil {
		t.Fatal(err)
	}
	a.waitExit(t)
	waitDead(t, aPID)
	dialOK(t, "127.0.0.1:"+bPort)
	list, err := runScript(t, env, "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, "alpha ") || !strings.Contains(list, "beta "+bPort) {
		t.Fatalf("list = %q", list)
	}
}

func TestParallelStartsGetDifferentPorts(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	env := testEnv(t, baseExtra(t, state, 39120, 39139, httpServer(t, dir)))
	agents := make([]*agent, 5)
	for i := 0; i < 5; i++ {
		agents[i] = startAgent(t, env, fmt.Sprintf("p%d", i), false)
	}
	for _, a := range agents {
		a.ready(t)
	}
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		port := instanceField(t, state, fmt.Sprintf("p%d", i), "port")
		n, err := strconv.Atoi(port)
		if err != nil || n < 39120 || n > 39139 {
			t.Fatalf("port %s out of range", port)
		}
		if seen[port] {
			t.Fatalf("duplicate port %s", port)
		}
		seen[port] = true
	}
}

func TestOccupiedPortIsNotKilled(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	ln, err := net.Listen("tcp", "127.0.0.1:39140")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	env := testEnv(t, baseExtra(t, state, 39140, 39149, httpServer(t, dir)))
	a := startAgent(t, env, "alpha", false)
	a.ready(t)
	port := instanceField(t, state, "alpha", "port")
	if port == "39140" {
		t.Fatal("agent took the occupied port")
	}
	dialOK(t, ln.Addr().String())
}

func TestPortTakenAfterReserveIsNotKilled(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	cmdPath := shellCommand(t, dir, "sleep 30\n")
	extra := baseExtra(t, state, 39150, 39159, cmdPath)
	extra["DEV_SERVER_READY_TIMEOUT"] = "10"
	env := testEnv(t, extra)
	cmd := exec.Command(scriptPath(t), "start", "alpha")
	cmd.Env = env
	var out safeBuf
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(finished)
	}()
	defer func() {
		select {
		case <-finished:
		default:
			_ = cmd.Process.Kill()
			<-finished
		}
	}()
	port := waitInstance(t, state, "alpha", "port")
	childRaw := waitInstance(t, state, "alpha", "go_pid")
	child, _ := strconv.Atoi(childRaw)
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	select {
	case <-finished:
		if cmd.ProcessState == nil || cmd.ProcessState.Success() {
			t.Fatalf("start succeeded\n%s", out.String())
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("start did not fail\n%s", out.String())
	}
	if !strings.Contains(out.String(), "left that process running") {
		t.Fatalf("output = %q", out.String())
	}
	dialOK(t, ln.Addr().String())
	waitDead(t, child)
}

func TestStaleRecordDoesNotKillUnrelatedProcess(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	env := testEnv(t, baseExtra(t, state, 39160, 39169, httpServer(t, dir)))
	sleepCmd := exec.Command("sleep", "30")
	if err := sleepCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sleepCmd.Process.Kill()
		_, _ = sleepCmd.Process.Wait()
	}()
	inst := filepath.Join(state, "alpha")
	if err := os.MkdirAll(inst, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("name=alpha\nport=39160\nurl=http://localhost:39160\nstatus=running\nsupervisor_pid=%d\ngo_pid=%d\nserver_pid=\nconfig=/tmp/dev-server-not-ours\nbinary=/tmp/dev-server-not-ours\nlog=/tmp/dev-server-not-ours.log\n", sleepCmd.Process.Pid, sleepCmd.Process.Pid)
	writeFile(t, filepath.Join(inst, "instance.env"), body, 0o644)

	list, err := runScript(t, env, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "alpha 39160 http://localhost:39160 stale") {
		t.Fatalf("list = %q", list)
	}
	out, err := runScript(t, env, "stop", "alpha")
	if err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not killing it") {
		t.Fatalf("stop output = %q", out)
	}
	if !pidAlive(sleepCmd.Process.Pid) {
		t.Fatal("unrelated process was killed")
	}
	if _, err := os.Stat(filepath.Join(inst, "instance.env")); !os.IsNotExist(err) {
		t.Fatal("stale record was not removed")
	}
}

func TestDeadRecordCanBeReplaced(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	env := testEnv(t, baseExtra(t, state, 39170, 39179, httpServer(t, dir)))
	sleepCmd := exec.Command("sleep", "30")
	if err := sleepCmd.Start(); err != nil {
		t.Fatal(err)
	}
	dead := sleepCmd.Process.Pid
	_ = sleepCmd.Process.Kill()
	_, _ = sleepCmd.Process.Wait()
	inst := filepath.Join(state, "alpha")
	if err := os.MkdirAll(inst, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("name=alpha\nport=39170\nurl=http://localhost:39170\nstatus=running\nsupervisor_pid=%d\ngo_pid=%d\nserver_pid=\nconfig=/tmp/dev-server-not-ours\nbinary=/tmp/dev-server-not-ours\nlog=/tmp/dev-server-not-ours.log\n", dead, dead)
	writeFile(t, filepath.Join(inst, "instance.env"), body, 0o644)

	a := startAgent(t, env, "alpha", false)
	a.ready(t)
	if !pidAlive(a.cmd.Process.Pid) {
		t.Fatal("replacement server is not running")
	}
}

func TestSignalsStopDetachedChild(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	server := httpServer(t, dir)
	cases := []struct {
		name    string
		sig     syscall.Signal
		group   bool
		minPort int
	}{
		{"SIGINT", syscall.SIGINT, false, 39180},
		{"SIGHUP", syscall.SIGHUP, false, 39184},
		{"SIGINT process group", syscall.SIGINT, true, 39188},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t, baseExtra(t, state, tc.minPort, tc.minPort+3, server))
			name := fmt.Sprintf("sig%d", tc.minPort)
			a := startAgent(t, env, name, tc.group)
			a.ready(t)
			child, _ := strconv.Atoi(instanceField(t, state, name, "go_pid"))
			var err error
			if tc.group {
				err = syscall.Kill(-a.cmd.Process.Pid, tc.sig)
			} else {
				err = a.cmd.Process.Signal(tc.sig)
			}
			if err != nil {
				t.Fatal(err)
			}
			a.waitExit(t)
			waitDead(t, child)
			if _, statErr := os.Stat(filepath.Join(state, name, "instance.env")); !os.IsNotExist(statErr) {
				t.Fatal("instance record was left behind")
			}
		})
	}
}

func TestEarlyExitShowsLog(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	cmdPath := shellCommand(t, dir, "echo boom-from-fake\nexit 7\n")
	env := testEnv(t, baseExtra(t, state, 39200, 39205, cmdPath))
	out, err := runScript(t, env, "start", "alpha")
	if err == nil {
		t.Fatalf("start succeeded\n%s", out)
	}
	if !strings.Contains(out, "exited before it was ready (status 7)") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "boom-from-fake") {
		t.Fatalf("log missing from output\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(state, "alpha", "instance.env")); !os.IsNotExist(statErr) {
		t.Fatal("failed start left an instance record")
	}
}

func TestReadyTimeoutStopsProcess(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	pidFile := filepath.Join(dir, "pid")
	cmdPath := shellCommand(t, dir, "echo $$ > \"$DEV_SERVER_ENV_OUT\"\necho still-starting\nsleep 30\n")
	extra := baseExtra(t, state, 39210, 39215, cmdPath)
	extra["DEV_SERVER_ENV_OUT"] = pidFile
	extra["DEV_SERVER_READY_TIMEOUT"] = "2"
	env := testEnv(t, extra)
	started := time.Now()
	out, err := runScript(t, env, "start", "alpha")
	if err == nil {
		t.Fatalf("start succeeded\n%s", out)
	}
	if time.Since(started) > 8*time.Second {
		t.Fatalf("timeout took %s\n%s", time.Since(started), out)
	}
	if !strings.Contains(out, "did not become ready within 2s") || !strings.Contains(out, "still-starting") {
		t.Fatalf("output = %q", out)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	waitDead(t, pid)
	if _, statErr := os.Stat(filepath.Join(state, "alpha", "instance.env")); !os.IsNotExist(statErr) {
		t.Fatal("timed out start left an instance record")
	}
}

func TestStopAllLeavesUnregisteredProcess(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	env := testEnv(t, baseExtra(t, state, 39220, 39229, httpServer(t, dir)))
	a := startAgent(t, env, "alpha", false)
	a.ready(t)
	child, _ := strconv.Atoi(instanceField(t, state, "alpha", "go_pid"))
	sleepCmd := exec.Command("sleep", "30")
	if err := sleepCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sleepCmd.Process.Kill()
		_, _ = sleepCmd.Process.Wait()
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:39229")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	out, err := runScript(t, env, "stop-all")
	if err != nil {
		t.Fatalf("stop-all: %v\n%s", err, out)
	}
	a.waitExit(t)
	waitDead(t, child)
	if !pidAlive(sleepCmd.Process.Pid) {
		t.Fatal("stop-all killed an unregistered process")
	}
	dialOK(t, ln.Addr().String())
	list, err := runScript(t, env, "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(list) != "no agent dev servers" {
		t.Fatalf("list = %q", list)
	}

	empty, err := runScript(t, env, "stop-all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(empty) != "no agent dev servers" {
		t.Fatalf("second stop-all = %q", empty)
	}
}

func TestGeneratedAirConfigPreservesBehavior(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	env := testEnv(t, baseExtra(t, state, 39230, 39235, httpServer(t, dir)))
	a := startAgent(t, env, "alpha", false)
	a.ready(t)
	got, err := os.ReadFile(filepath.Join(state, "alpha", "air.toml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(repoRoot(t), ".air.toml"))
	if err != nil {
		t.Fatal(err)
	}
	keep := []string{
		`include_ext = ["go", "html", "css", "js", "svg", "webmanifest"]`,
		`exclude_dir = ["tmp", "bin", "data", "vendor", "testdata", ".git"]`,
		`exclude_regex = ["_test\\.go"]`,
		"send_interrupt = true",
		"kill_delay = 1000",
	}
	for _, line := range keep {
		if !strings.Contains(string(want), line) {
			t.Fatalf(".air.toml no longer contains %s", line)
		}
		if !strings.Contains(string(got), line) {
			t.Fatalf("generated config lost %s\n%s", line, got)
		}
	}
	binary := filepath.Join(state, "alpha", "air", "main")
	tmpDir := filepath.Join(state, "alpha", "air")
	if !strings.Contains(string(got), `tmp_dir = "`+tmpDir+`"`) {
		t.Fatalf("tmp_dir = %s", got)
	}
	if !strings.Contains(string(got), "CGO_ENABLED=0") || !strings.Contains(string(got), binary) {
		t.Fatalf("cmd = %s", got)
	}
	if strings.Contains(string(got), "./tmp/main") {
		t.Fatalf("generated config still builds ./tmp/main\n%s", got)
	}
	if strings.Contains(string(got), "include_dir") {
		t.Fatalf("generated config added include_dir\n%s", got)
	}
}

func TestReloadDirIsOptIn(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	reload := filepath.Join(dir, "reload")
	extra := baseExtra(t, state, 39240, 39245, httpServer(t, dir))
	extra["DEV_SERVER_RELOAD_DIR"] = reload
	env := testEnv(t, extra)
	a := startAgent(t, env, "alpha", false)
	a.ready(t)
	got, err := os.ReadFile(filepath.Join(state, "alpha", "air.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `include_dir = ["`+reload+`"]`) {
		t.Fatalf("config = %s", got)
	}
	if _, err := os.Stat(reload); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsBadNamesAndMissingStop(t *testing.T) {
	dir := t.TempDir()
	env := testEnv(t, baseExtra(t, filepath.Join(dir, "state"), 39250, 39255, "true"))
	for _, name := range []string{"", "../x", "has space", "-bad", "this-name-is-longer-than-thirty-two"} {
		out, err := runScript(t, env, "start", name)
		if err == nil {
			t.Fatalf("start %q succeeded\n%s", name, out)
		}
	}
	out, err := runScript(t, env, "stop", "missing")
	if err == nil {
		t.Fatalf("stop missing succeeded\n%s", out)
	}
	if !strings.Contains(out, "not running") {
		t.Fatalf("stop output = %q", out)
	}
}

func TestMakefileDevIsPersonalServer(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	personal := "ADDR=:8200 PORT=8200 BASE_URL=http://localhost:8200 "
	if !strings.Contains(text, "dev:\n\t"+personal+"go run github.com/air-verse/air@v1.67.4\n") {
		t.Fatalf("make dev changed:\n%s", text)
	}
	if !strings.Contains(text, "run:\n\t"+personal+"go run .\n") {
		t.Fatalf("make run changed:\n%s", text)
	}
	if strings.Contains(text, "docker run") && strings.Contains(text, "-e PORT=") {
		t.Fatal("docker-run must not pass PORT into the container")
	}
	if !strings.Contains(text, "-p 8200:8080") {
		t.Fatal("docker-run must publish host 8200 to container 8080")
	}
	script, err := os.ReadFile(filepath.Join(root, "scripts", "dev-server.sh"))
	if err != nil {
		t.Fatal(err)
	}
	scriptText := string(script)
	if !strings.Contains(scriptText, "PORT_MIN:-8300") || !strings.Contains(scriptText, "PORT_MAX:-8399") || !strings.Contains(scriptText, "READY_TIMEOUT:-60") {
		t.Fatal("dev-server defaults changed")
	}
	if strings.Contains(scriptText, "pkill") || strings.Contains(scriptText, "killall") {
		t.Fatal("dev-server must not kill by name")
	}
}

func TestAgentOverridesPersonalPaths(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	envOut := filepath.Join(dir, "env.txt")
	root := repoRoot(t)
	extra := baseExtra(t, state, 39300, 39309, httpServer(t, dir))
	extra["DEV_SERVER_ENV_OUT"] = envOut
	extra["ADDR"] = ":8200"
	extra["PORT"] = "8200"
	extra["BASE_URL"] = "http://localhost:8200"
	extra["DB_PATH"] = filepath.Join(root, "data", "djm.db")
	extra["BLOB_DIR"] = filepath.Join(root, "data", "blobs")
	extra["CATALOG_CACHE_DIR"] = filepath.Join(root, "data", "catalogs")
	env := testEnv(t, extra)

	agent := startAgent(t, env, "alpha", false)
	agent.ready(t)
	port := instanceField(t, state, "alpha", "port")
	if port == "8200" || port == "8080" {
		t.Fatalf("agent port = %s", port)
	}
	body, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 6 {
		t.Fatalf("env lines = %q", lines)
	}
	if lines[0] != ":"+port || lines[1] != port || lines[3] != "http://localhost:"+port {
		t.Fatalf("listen env = %q", lines)
	}
	if lines[2] != filepath.Join(state, "alpha", "data", "djm.db") {
		t.Fatalf("DB_PATH = %s", lines[2])
	}
	if lines[4] != filepath.Join(state, "alpha", "blobs") || lines[5] != filepath.Join(state, "alpha", "catalogs") {
		t.Fatalf("dirs = %s %s", lines[4], lines[5])
	}
	assertNotPersonal(t, lines[2], lines[4], lines[5])
}

func TestReservedPortsDoNotLaunch(t *testing.T) {
	spans := [][2]int{{8080, 8080}, {8200, 8200}, {8100, 8199}}
	for _, span := range spans {
		t.Run(fmt.Sprintf("%d-%d", span[0], span[1]), func(t *testing.T) {
			dir := t.TempDir()
			env := testEnv(t, baseExtra(t, filepath.Join(dir, "state"), span[0], span[1], "true"))
			out, err := runScript(t, env, "start", "alpha")
			if err == nil {
				t.Fatalf("start succeeded\n%s", out)
			}
			if strings.Contains(out, "dev-agent: starting") || !strings.Contains(out, "no free port") {
				t.Fatalf("output = %q", out)
			}
		})
	}
}

func TestRefusesPersonalDataDirectory(t *testing.T) {
	root := repoRoot(t)
	state := filepath.Join(root, "data", "dev-agent-refusal-test")
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	env := testEnv(t, baseExtra(t, state, 39320, 39329, "true"))
	out, err := runScript(t, env, "start", "alpha")
	if err == nil {
		t.Fatalf("start succeeded\n%s", out)
	}
	if !strings.Contains(out, "personal data directory") {
		t.Fatalf("output = %q", out)
	}
	if _, statErr := os.Stat(state); !os.IsNotExist(statErr) {
		t.Fatalf("created %s: %v", state, statErr)
	}
}

func TestReservedPortsAreNotLaunched(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	cmd := shellCommand(t, dir, "touch \"$DEV_SERVER_ENV_OUT\"\nsleep 30\n")
	cases := []struct {
		name     string
		min, max int
	}{
		{"track-anything-personal", 8080, 8080},
		{"track-anything-agents", 8100, 8199},
		{"djm-personal", 8200, 8200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			extra := baseExtra(t, filepath.Join(dir, tc.name), tc.min, tc.max, cmd)
			extra["DEV_SERVER_ENV_OUT"] = marker
			env := testEnv(t, extra)
			out, err := runScript(t, env, "start", "alpha")
			if err == nil {
				t.Fatalf("start succeeded\n%s", out)
			}
			if !strings.Contains(out, "no free port") {
				t.Fatalf("output = %q", out)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("reserved range %d-%d launched the child\n%s", tc.min, tc.max, out)
			}
		})
	}
}
