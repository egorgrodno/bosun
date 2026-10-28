package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// gitRepos answers the gate from git and from the git setups recorded in dir, one file per repository.
type gitRepos struct{ dir string }

// gitSetup is what in a repository decides where a push goes and what runs on the way. A worker's sandbox may write all of it.
type gitSetup struct {
	Root string `json:"root"`
	// Config holds the repository's own config entries, each a key and its value a newline apart, but for branch.* ones.
	Config []string `json:"config"`
	// Hooks maps each file in the hooks directory to its mode and the hash of its content.
	Hooks map[string]string `json:"hooks"`
	// Remotes maps each remote to its push URLs.
	Remotes map[string][]string `json:"remotes"`
	// branchRemotes are the remotes that branch.* entries name, where a push that names none goes.
	branchRemotes []string
}

func (r gitRepos) path(root string) string {
	return filepath.Join(r.dir, notAlnum.ReplaceAllString(root, "-")+".json")
}

func (r gitRepos) remotes(dir string) []string {
	out, err := gitRead(dir, "remote")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// record writes down the git setup of dir's repository, unless a record of it exists.
func (r gitRepos) record(dir string) error {
	s, err := readSetup(dir)
	if err != nil {
		return err
	}
	path := r.path(s.Root)
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	return writeJSON(path, s)
}

// load reads the git setup of dir's repository and bosun's record of it, which is the zero gitSetup when bosun has none. why says what keeps it from either, completing "a repository …".
func (r gitRepos) load(dir string) (rec, s gitSetup, why string) {
	s, err := readSetup(dir)
	if err != nil {
		log.Printf("reading the git setup in %s: %v", dir, err)
		return rec, s, "bosun cannot read"
	}
	data, err := os.ReadFile(r.path(s.Root))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil || json.Unmarshal(data, &rec) != nil || rec.Root != s.Root:
		return gitSetup{}, s, "whose record bosun cannot read"
	}
	return rec, s, ""
}

func (r gitRepos) changed(dir string) string {
	rec, s, why := r.load(dir)
	switch {
	case why != "":
		return why
	case rec.Root == "":
		return "bosun has no record of"
	}
	return rec.changed(s)
}

// hooks says how the hooks a push from dir runs differ from bosun's record of its repository, completing "a repository …", or returns "".
// A repository without a record must set no hook in its own config. The hooks in its hooks directory run after the user's own.
func (r gitRepos) hooks(dir string) string {
	rec, s, why := r.load(dir)
	if why != "" {
		return why
	}
	if rec.Root == "" {
		rec.Hooks = s.Hooks
	}
	return rec.hookSetup().changed(s.hookSetup())
}

// hookSetup is s cut to what decides the hooks a push runs: the hooks directory, and the config entries that hookKey names.
func (s gitSetup) hookSetup() gitSetup {
	return gitSetup{Hooks: s.Hooks, Config: slices.DeleteFunc(slices.Clone(s.Config), func(e string) bool {
		key, _, _ := strings.Cut(e, "\n")
		return !hookKey(key)
	})}
}

// changed says how s differs from the record rec, completing "a repository …", or returns "".
func (rec gitSetup) changed(s gitSetup) string {
	for _, e := range slices.Sorted(slices.Values(append(slices.Clone(rec.Config), s.Config...))) {
		if slices.Contains(rec.Config, e) != slices.Contains(s.Config, e) {
			key, _, _ := strings.Cut(e, "\n")
			return fmt.Sprintf("whose config's %s changed", key)
		}
	}
	for _, name := range append(slices.Sorted(maps.Keys(rec.Hooks)), slices.Sorted(maps.Keys(s.Hooks))...) {
		if rec.Hooks[name] != s.Hooks[name] {
			return fmt.Sprintf("whose hook %s changed", name)
		}
	}
	for _, name := range append(slices.Sorted(maps.Keys(rec.Remotes)), slices.Sorted(maps.Keys(s.Remotes))...) {
		if !slices.Equal(rec.Remotes[name], s.Remotes[name]) {
			return fmt.Sprintf("whose remote %s changed", name)
		}
	}
	for _, name := range s.branchRemotes {
		// A branch tracking another local branch pushes to the repository itself.
		if _, ok := rec.Remotes[name]; !ok && name != "." {
			return fmt.Sprintf("whose branch config names %s, not a recorded remote", name)
		}
	}
	return ""
}

// readSetup reads the git setup of dir's repository as a push from dir sees it.
func readSetup(dir string) (gitSetup, error) {
	s := gitSetup{Hooks: map[string]string{}, Remotes: map[string][]string{}}
	out, err := gitRead(dir, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel")
	paths := strings.Split(out, "\n")
	if err != nil || len(paths) != 2 {
		return s, cmp.Or(err, fmt.Errorf("git rev-parse printed %q", out))
	}
	s.Root = filepath.Dir(paths[0])

	out, err = gitRead(dir, "config", "--list", "--includes", "--show-scope", "-z")
	if err != nil {
		return s, err
	}
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		scope, entry := fields[i], fields[i+1]
		if scope != "local" && scope != "worktree" {
			continue
		}
		key, value, _ := strings.Cut(entry, "\n")
		switch {
		case !strings.HasPrefix(key, "branch."):
			s.Config = append(s.Config, entry)
		case strings.HasSuffix(key, ".remote"), strings.HasSuffix(key, ".pushremote"):
			s.branchRemotes = append(s.branchRemotes, value)
		}
	}
	slices.Sort(s.Config)

	// git runs hooks from the top of the worktree, which a relative core.hooksPath starts from.
	hooks, err := gitRead(paths[1], "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		return s, err
	}
	entries, err := os.ReadDir(hooks)
	// core.hooksPath=/dev/null turns every hook off.
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
		return s, err
	}
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(hooks, e.Name()))
		if err != nil {
			return s, err
		}
		s.Hooks[e.Name()] = info.Mode().String()
		// Reading a pipe a worker left there would wait for good.
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(filepath.Join(hooks, e.Name()))
			if err != nil {
				return s, err
			}
			s.Hooks[e.Name()] += fmt.Sprintf(" %x", sha256.Sum256(data))
		}
	}

	out, err = gitRead(dir, "remote")
	if err != nil {
		return s, err
	}
	for name := range strings.FieldsSeq(out) {
		urls, err := gitRead(dir, "remote", "get-url", "--push", "--all", name)
		if err != nil {
			return s, err
		}
		s.Remotes[name] = strings.Split(urls, "\n")
	}
	return s, nil
}

// gitRead runs git in dir to read its repository. It gives up after a while, since a worker's config could make git wait, as on a pipe it includes.
func gitRead(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
