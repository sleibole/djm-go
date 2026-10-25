package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func setupScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), ".cursor", "setup-worktree-unix.sh")
}

func gitEnv(env []string) []string {
	return append(env,
		"GIT_AUTHOR_NAME=djm test",
		"GIT_AUTHOR_EMAIL=djm-test@example.com",
		"GIT_COMMITTER_NAME=djm test",
		"GIT_COMMITTER_EMAIL=djm-test@example.com",
	)
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func copyRepoFile(t *testing.T, src, dst string) {
	t.Helper()
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o644)
	if strings.HasSuffix(src, ".sh") {
		mode = 0o755
	}
	if err := os.WriteFile(dst, body, mode); err != nil {
		t.Fatal(err)
	}
}

func plantCheckout(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "README"), "temp checkout\n", 0o644)
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "add", "README")
	gitCmd(t, dir, "-c", "commit.gpgsign=false", "commit", "-m", "init")
	root := repoRoot(t)
	copyRepoFile(t, filepath.Join(root, "go.mod"), filepath.Join(dir, "go.mod"))
	copyRepoFile(t, filepath.Join(root, "go.sum"), filepath.Join(dir, "go.sum"))
	copyRepoFile(t, filepath.Join(root, ".air.toml"), filepath.Join(dir, ".air.toml"))
	copyRepoFile(t, setupScript(t), filepath.Join(dir, ".cursor", "setup-worktree-unix.sh"))
	copyRepoFile(t, scriptPath(t), filepath.Join(dir, "scripts", "dev-server.sh"))
}

func runSetup(t *testing.T, worktree, primary string) string {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(worktree, ".cursor", "setup-worktree-unix.sh"))
	cmd.Dir = worktree
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ROOT_WORKTREE_PATH=") {
			continue
		}
		env = append(env, kv)
	}
	if primary != "" {
		env = append(env, "ROOT_WORKTREE_PATH="+primary)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	return string(out)
}

func worktreeID(t *testing.T, output string) string {
	t.Helper()
	match := regexp.MustCompile(`worktree_id: (wt-[0-9a-f]{12})`).FindStringSubmatch(output)
	if match == nil {
		t.Fatalf("output has no worktree id:\n%s", output)
	}
	return match[1]
}

func TestSetupWorktreeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	plantCheckout(t, dir)
	first := runSetup(t, dir, "")
	second := runSetup(t, dir, "")
	if worktreeID(t, first) != worktreeID(t, second) {
		t.Fatalf("ids changed:\n%s\n%s", first, second)
	}
	for _, output := range []string{first, second} {
		if !strings.Contains(output, "server: not started") || strings.Contains(output, "dev-agent: ready") {
			t.Fatalf("setup started a server:\n%s", output)
		}
		if !strings.Contains(output, "no .env in this worktree") {
			t.Fatalf("setup did not describe the missing env:\n%s", output)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "tmp"))
	if err != nil || !info.IsDir() {
		t.Fatalf("tmp = %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env was created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Fatalf("personal data directory was created: %v", err)
	}
}

func TestSetupLeavesExistingEnv(t *testing.T) {
	dir := t.TempDir()
	plantCheckout(t, dir)
	const secret = "smtp-pass-sentinel-do-not-print"
	body := "SMTP_PASS=" + secret + "\nDEV_SHOW_MAGIC_LINK=true\n"
	writeFile(t, filepath.Join(dir, ".env"), body, 0o600)
	output := runSetup(t, dir, "")
	got, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf(".env changed to %q", got)
	}
	if strings.Contains(output, secret) {
		t.Fatal("setup printed the environment file")
	}
	if !strings.Contains(output, "left it unchanged") {
		t.Fatalf("output = %s", output)
	}
}

func TestSetupDoesNotCopySecrets(t *testing.T) {
	dir := t.TempDir()
	plantCheckout(t, dir)
	primary := t.TempDir()
	const secret = "aws-secret-sentinel-do-not-print"
	primaryEnv := "SMTP_PASS=" + secret + "\nAWS_SECRET_ACCESS_KEY=" + secret + "\n"
	writeFile(t, filepath.Join(primary, ".env"), primaryEnv, 0o600)
	marker := filepath.Join(primary, "data", "djm.db")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, marker, "personal-db", 0o644)
	primaryTmp := filepath.Join(primary, "tmp", "dev-agents", "keep")
	if err := os.MkdirAll(primaryTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(primaryTmp, "marker"), "keep", 0o644)

	output := runSetup(t, dir, primary)
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Fatalf("worktree gained .env: %v", err)
	}
	if strings.Contains(output, secret) {
		t.Fatal("setup printed a secret from the primary checkout")
	}
	got, err := os.ReadFile(filepath.Join(primary, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != primaryEnv {
		t.Fatalf("primary .env changed to %q", got)
	}
	markerBody, err := os.ReadFile(marker)
	if err != nil || string(markerBody) != "personal-db" {
		t.Fatalf("primary database changed: %v %q", err, markerBody)
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp", "dev-agents")); !os.IsNotExist(err) {
		t.Fatalf("setup created an agent state directory: %v", err)
	}
}

func TestSetupRequiresGit(t *testing.T) {
	empty := t.TempDir()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, setupScript(t))
	cmd.Env = []string{"PATH=" + empty, "HOME=" + empty}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("setup succeeded without tools\n%s", out)
	}
	if !strings.Contains(string(out), "git is required") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(string(out), "dev-agent: ready") {
		t.Fatalf("setup started a server\n%s", out)
	}
}

func TestTwoGitWorktreesIsolateServers(t *testing.T) {
	parent := t.TempDir()
	origin := filepath.Join(parent, "origin")
	wtA := filepath.Join(parent, "wt-a")
	wtB := filepath.Join(parent, "wt-b")
	plantCheckout(t, origin)
	gitCmd(t, origin, "worktree", "add", "--detach", wtA)
	gitCmd(t, origin, "worktree", "add", "--detach", wtB)
	t.Cleanup(func() {
		remove := exec.Command("git", "worktree", "remove", "--force", wtA)
		remove.Dir = origin
		_ = remove.Run()
		remove = exec.Command("git", "worktree", "remove", "--force", wtB)
		remove.Dir = origin
		_ = remove.Run()
	})
	root := repoRoot(t)
	for _, wt := range []string{wtA, wtB} {
		copyRepoFile(t, filepath.Join(root, "go.mod"), filepath.Join(wt, "go.mod"))
		copyRepoFile(t, filepath.Join(root, "go.sum"), filepath.Join(wt, "go.sum"))
		copyRepoFile(t, filepath.Join(root, ".air.toml"), filepath.Join(wt, ".air.toml"))
		copyRepoFile(t, scriptPath(t), filepath.Join(wt, "scripts", "dev-server.sh"))
		copyRepoFile(t, setupScript(t), filepath.Join(wt, ".cursor", "setup-worktree-unix.sh"))
		first := runSetup(t, wt, origin)
		second := runSetup(t, wt, origin)
		if worktreeID(t, first) != worktreeID(t, second) {
			t.Fatalf("%s ids changed", wt)
		}
		if strings.Contains(first, "dev-agent: ready") {
			t.Fatalf("%s setup started a server\n%s", wt, first)
		}
		if _, err := os.Stat(filepath.Join(wt, "data")); !os.IsNotExist(err) {
			t.Fatalf("%s created data/: %v", wt, err)
		}
	}

	server := httpServer(t, parent)
	envFor := func(wt, envOut string, min, max int) []string {
		return testEnv(t, map[string]string{
			"DEV_SERVER_PORT_MIN":      strconv.Itoa(min),
			"DEV_SERVER_PORT_MAX":      strconv.Itoa(max),
			"DEV_SERVER_READY_TIMEOUT": "15",
			"DEV_SERVER_COMMAND":       server,
			"DEV_SERVER_SCRIPT":        filepath.Join(wt, "scripts", "dev-server.sh"),
			"DEV_SERVER_ENV_OUT":       envOut,
		})
	}
	envA := envFor(wtA, filepath.Join(parent, "a-env.txt"), 39680, 39699)
	envB := envFor(wtB, filepath.Join(parent, "b-env.txt"), 39680, 39699)
	agentA := startAgent(t, envA, "agent", false)
	agentB := startAgent(t, envB, "agent", false)
	agentA.ready(t)
	agentB.ready(t)

	stateA := filepath.Join(wtA, "tmp", "dev-agents")
	stateB := filepath.Join(wtB, "tmp", "dev-agents")
	portA := instanceField(t, stateA, "agent", "port")
	portB := instanceField(t, stateB, "agent", "port")
	if portA == portB {
		t.Fatalf("both worktrees used port %s", portA)
	}
	envLines := func(path, wt string) []string {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if len(lines) != 6 {
			t.Fatalf("env lines = %q", lines)
		}
		resolved, err := filepath.EvalSymlinks(wt)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range lines[2:] {
			if line == lines[1] {
				continue
			}
			if !strings.Contains(line, resolved) && !strings.Contains(line, wt) && !strings.HasPrefix(line, "http://") {
				t.Errorf("path %s is outside %s", line, wt)
			}
		}
		assertNotPersonal(t, lines[2], lines[4], lines[5])
		return lines
	}
	linesA := envLines(filepath.Join(parent, "a-env.txt"), wtA)
	linesB := envLines(filepath.Join(parent, "b-env.txt"), wtB)
	if linesA[2] == linesB[2] || linesA[4] == linesB[4] || linesA[5] == linesB[5] {
		t.Fatalf("worktrees share storage\n%s\n%s", linesA, linesB)
	}
	dialOK(t, "127.0.0.1:"+portA)
	dialOK(t, "127.0.0.1:"+portB)

	commonCmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	commonCmd.Dir = wtA
	commonOut, err := commonCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	common := strings.TrimSpace(string(commonOut))
	if _, err := os.Stat(filepath.Join(common, "djm-dev-ports", portA, "owner")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(common, "djm-dev-ports", portB, "owner")); err != nil {
		t.Fatal(err)
	}
	assertPortNotInGitRegistry(t, portA)
	assertPortNotInGitRegistry(t, portB)

	resolvedB, err := filepath.EvalSymlinks(wtB)
	if err != nil {
		t.Fatal(err)
	}
	list, err := runScript(t, envA, "list")
	if err != nil {
		t.Fatal(err, list)
	}
	if !strings.Contains(list, "agent "+portA+" http://localhost:"+portA+" running") {
		t.Fatalf("list missed the local server\n%s", list)
	}
	if !strings.Contains(list, "agent "+portB+" http://localhost:"+portB+" external "+resolvedB) &&
		!strings.Contains(list, "agent "+portB+" http://localhost:"+portB+" external "+wtB) {
		t.Fatalf("list missed the other worktree\n%s", list)
	}

	if _, err := runScript(t, envA, "stop", "agent"); err != nil {
		t.Fatal(err)
	}
	agentA.waitExit(t)
	if _, err := os.Stat(filepath.Join(common, "djm-dev-ports", portA)); !os.IsNotExist(err) {
		t.Fatalf("stopped worktree left claim %s: %v", portA, err)
	}
	dialOK(t, "127.0.0.1:"+portB)
	if _, err := os.Stat(filepath.Join(common, "djm-dev-ports", portB, "owner")); err != nil {
		t.Fatal(err)
	}
	list, err = runScript(t, envB, "list")
	if err != nil {
		t.Fatal(err, list)
	}
	if strings.Contains(list, "external") || !strings.Contains(list, "agent "+portB+" ") {
		t.Fatalf("list after stopping the other worktree = %q", list)
	}
}
