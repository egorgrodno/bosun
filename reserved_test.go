package main

import (
	"encoding/json"
	"testing"
)

func TestReservedCommand(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{"git push", "git push"},
		{"git push --force origin main", "git push"},
		{"git -C /repo push", "git push"},
		{"git -c user.name=x push", "git push"},
		{"/usr/bin/git push", "git push"},
		{"cd sub && git push", "git push"},
		{"make; git push origin HEAD", "git push"},
		{"FOO=1 git push", "git push"},
		{"sudo git push", "git push"},
		{"bash -c 'git push'", "git push"},
		{"sh -ec \"git push\"", "git push"},
		{"eval git push", "git push"},
		{`echo "$(git push)"`, "git push"},
		{"echo `git push`", "git push"},
		{"(git push)", "git push"},
		{"git branch -D old", "deleting a branch"},
		{"git branch -d old", "deleting a branch"},
		{"git branch --delete old", "deleting a branch"},
		{"git update-ref -d refs/heads/old", "deleting a branch"},
		{"git status", ""},
		{"git stash push", ""},
		{"git branch -m old new", ""},
		{"git branch --list", ""},
		{`git commit -m "do not git push yet"`, ""},
		{"git commit -m 'push'", ""},
		{"git log | grep push", ""},
		{"echo hi # git push", ""},
		{"ls -la", ""},
	} {
		if got := reservedCommand(tc.cmd); got != tc.want {
			t.Errorf("reservedCommand(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

func TestReservedWrite(t *testing.T) {
	const (
		wt     = "/repo/.claude/worktrees/a"
		memory = "/home/u/.claude/projects/-repo/memory"
		job    = "/home/u/.claude/jobs/aaaaaaaa"
	)
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
		if got := reservedAction(tc.tool, input, wt, memory, job+"/tmp") != ""; got != tc.reserved {
			t.Errorf("%s %s: reserved = %v, want %v", tc.tool, tc.path, got, tc.reserved)
		}
	}
	input, _ := json.Marshal(map[string]string{"notebook_path": "/tmp/n.ipynb"})
	if reservedAction("NotebookEdit", input, wt) == "" {
		t.Error("NotebookEdit outside the worktree is not reserved")
	}
}

func TestReservedUnsandboxed(t *testing.T) {
	for _, tc := range []struct {
		input map[string]any
		want  string
	}{
		{map[string]any{"command": "ls", "dangerouslyDisableSandbox": true}, "a command outside the sandbox"},
		{map[string]any{"command": "git push", "dangerouslyDisableSandbox": true}, "git push"},
		{map[string]any{"command": "ls", "dangerouslyDisableSandbox": false}, ""},
		{map[string]any{"command": "curl example.com", "allowed_domains": []string{"example.com"}}, ""},
	} {
		input, _ := json.Marshal(tc.input)
		if got := reservedAction("Bash", input, "/wt"); got != tc.want {
			t.Errorf("Bash %v: reserved = %q, want %q", tc.input, got, tc.want)
		}
	}
}
