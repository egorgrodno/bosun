package main

import (
	"cmp"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// gate checks a session's tool calls for actions only the user may approve.
type gate struct {
	// cwd is where the call runs.
	cwd string
	// writable lists where the session's file tools may write, its worktree first.
	writable []string
	repos    repos
}

// repos is what the gate asks of the repository a git push runs in.
type repos interface {
	// remotes lists the remotes configured in dir's repository.
	remotes(dir string) []string
	// changed says how dir's repository differs from bosun's record of its git setup, completing "a repository …", or returns "".
	changed(dir string) string
}

// rebuildRules make every rebuild that the allowlist lets out of the sandbox reach the master, which auto mode would otherwise decide alone.
// Each command rebuild allows must match one of them.
var rebuildRules = []string{"Bash(nixos-rebuild:*)", "Bash(nxt)", "Bash(nxc)"}

// reserved names what in a tool call only the user may approve, or returns "".
func (g gate) reserved(tool string, input json.RawMessage) string {
	switch tool {
	case "Bash":
		var in struct {
			Command string `json:"command"`
			Unboxed bool   `json:"dangerouslyDisableSandbox"`
		}
		if json.Unmarshal(input, &in) == nil {
			if r := g.command(in.Command, []string{g.cwd}); r != "" || !in.Unboxed {
				return r
			}
			// Auto mode approves running outside the sandbox without asking.
			return g.unboxed(in.Command)
		}
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		var in struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		}
		_ = json.Unmarshal(input, &in)
		if p := cmp.Or(in.FilePath, in.NotebookPath); p != "" && !within(g.writable, p) {
			return "a write outside the worktree"
		}
	}
	return ""
}

// within reports whether path lies in one of dirs, resolving a relative path against the first.
func within(dirs []string, path string) bool {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dirs[0], path)
	}
	return slices.ContainsFunc(dirs, func(dir string) bool {
		rel, err := filepath.Rel(dir, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
	})
}

// command checks a script that may run in each of dirs. Each cd adds where it leads, or "" when bosun cannot tell.
func (g gate) command(script string, dirs []string) string {
	for _, argv := range simpleCommands(script) {
		if r := g.argv(argv, dirs); r != "" {
			return r
		}
		if argv[0] == "cd" || argv[0] == "pushd" {
			dirs = append(slices.Clip(dirs), chdir(dirs[len(dirs)-1], argv[1:]))
		}
	}
	return ""
}

// argv looks for git anywhere in a command, so that wrappers such as sudo, env or xargs need no rules of their own.
func (g gate) argv(argv, dirs []string) string {
	for i, w := range argv {
		switch filepath.Base(w) {
		case "git":
			if r := g.git(argv[i+1:], dirs); r != "" {
				return r
			}
		case "eval":
			return g.command(strings.Join(argv[i+1:], " "), dirs)
		case "sh", "bash", "zsh", "dash":
			for j := i + 1; j+1 < len(argv); j++ {
				if a := argv[j]; len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'c') {
					return g.command(argv[j+1], dirs)
				}
			}
		}
	}
	return ""
}

func (g gate) git(args, dirs []string) string {
	cd, sub, rest, plain := gitCommand(args)
	switch sub {
	case "push":
		if !plain {
			return "a push with an option bosun does not check"
		}
		named, r := pushArgs(rest)
		if r != "" {
			return r
		}
		for _, name := range named {
			for _, dir := range dirs {
				if dir == "" || !slices.Contains(g.repos.remotes(at(dir, cd)), name) {
					return "a push to a URL or an unknown remote"
				}
			}
		}
	case "branch":
		for _, a := range rest {
			// git takes any unambiguous prefix of a long option, such as --del.
			if len(a) > 2 && strings.HasPrefix("--delete", a) || len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a, "dD") {
				return "deleting a branch"
			}
		}
	case "update-ref":
		// --stdin reads commands that may delete any ref.
		if slices.Contains(rest, "-d") || slices.ContainsFunc(rest, func(a string) bool { return len(a) > 2 && strings.HasPrefix("--stdin", a) }) {
			return "deleting a branch"
		}
	}
	return ""
}

// gitCommand splits git's arguments into the directories its -C options name, its subcommand and the subcommand's arguments. plain reports whether git has no other option, any of which could move the repository or its config.
func gitCommand(args []string) (cd []string, sub string, rest []string, plain bool) {
	plain = true
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch args[i] {
		case "-C":
			if i+1 < len(args) {
				cd = append(cd, args[i+1])
			}
			i++
			continue
		case "-c", "--git-dir", "--work-tree", "--namespace", "--config-env":
			i++
		}
		plain = false
	}
	if i >= len(args) {
		return cd, "", nil, plain
	}
	return cd, args[i], args[i+1:], plain
}

// pushFlags are git push's options that take no value and change neither where it pushes nor what it may overwrite.
// git takes any unambiguous prefix of a long option, so one missing here counts as unknown.
var pushFlags = []string{"--set-upstream", "--tags", "--follow-tags", "--dry-run", "--porcelain", "--verbose", "--quiet", "--progress", "--no-progress", "--verify", "--no-verify", "--atomic", "--ipv4", "--ipv6"}

// pushArgs returns the repositories git push's arguments name, or what in them only the user may approve.
func pushArgs(args []string) (repos []string, reason string) {
	var pos []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
		case a == "--force", a == "--force-with-lease", strings.HasPrefix(a, "--force-with-lease="), a == "--force-if-includes":
			return nil, "a force push"
		case a == "--delete", a == "--prune":
			return nil, "a push that deletes a ref"
		case a == "--all", a == "--branches":
			return nil, "a push of every branch"
		case a == "--mirror":
			return nil, "a mirror push"
		case a == "--repo" && i+1 < len(args):
			i++
			repos = append(repos, args[i])
		case strings.HasPrefix(a, "--repo="):
			repos = append(repos, strings.TrimPrefix(a, "--repo="))
		case a == "--push-option":
			i++
		case strings.HasPrefix(a, "--push-option="), slices.Contains(pushFlags, a):
		case strings.HasPrefix(a, "--"):
			return nil, "a push with an option bosun does not check"
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'f':
					return nil, "a force push"
				case 'd':
					return nil, "a push that deletes a ref"
				case 'o':
					// The rest of the word is -o's value, or the next word when nothing follows.
					if j == len(a)-1 {
						i++
					}
					j = len(a)
				case 'u', 'q', 'v', 'n', '4', '6':
				default:
					return nil, "a push with an option bosun does not check"
				}
			}
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) == 0 {
		return repos, ""
	}
	for _, ref := range pos[1:] {
		switch {
		case strings.HasPrefix(ref, "+"):
			return nil, "a force push"
		case ref == ":" || strings.Contains(ref, "*"):
			return nil, "a push of every branch"
		case strings.HasPrefix(ref, ":"):
			return nil, "a push that deletes a ref"
		}
	}
	return append(repos, pos[0]), ""
}

// at is where git started in dir runs with -C options cd.
func at(dir string, cd []string) string {
	for _, c := range cd {
		if filepath.IsAbs(c) {
			dir = filepath.Clean(c)
		} else if dir != "" {
			dir = filepath.Join(dir, c)
		}
	}
	return dir
}

// chdir is where cd with args leads from dir, or "" when bosun cannot tell.
func chdir(dir string, args []string) string {
	if len(args) != 1 || args[0] == "" || args[0][0] == '-' || strings.ContainsAny(args[0], "$~*?[{") {
		return ""
	}
	return at(dir, args)
}

// unboxed checks a command outside the sandbox that command found nothing in. The master may decide only a whole command of plain words on the allowlist: a rebuild that adds no boot entry, or a push from a repository whose git setup, which runs unsandboxed on the way, matches bosun's record.
func (g gate) unboxed(script string) string {
	argv := strings.Split(script, " ")
	if rebuild(argv) {
		return ""
	}
	if !slices.ContainsFunc(argv, func(w string) bool { return !plainWord.MatchString(w) }) && argv[0] == "git" {
		if cd, sub, _, plain := gitCommand(argv[1:]); sub == "push" && plain {
			if r := g.repos.changed(at(g.cwd, cd)); r != "" {
				return "a push outside the sandbox from a repository " + r
			}
			return ""
		}
	}
	return "a command outside the sandbox"
}

// plainWord matches a word that neither bash nor zsh expands, even with zsh's extended globs, where # and ~ are operators.
var plainWord = regexp.MustCompile(`^[A-Za-z0-9_./:@+-][A-Za-z0-9_./:@+=-]*$`)

// rebuild reports whether argv tests or builds the NixOS configuration in /etc/nixos or a worktree of it.
// nxt and nxc run nixos-rebuild test and build on /etc/nixos.
func rebuild(argv []string) bool {
	if len(argv) == 1 {
		return argv[0] == "nxt" || argv[0] == "nxc"
	}
	if argv[0] != "nixos-rebuild" {
		return false
	}
	var action, flake, sudo bool
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; {
		case (a == "test" || a == "build") && !action:
			action = true
		case a == "--sudo" && !sudo:
			sudo = true
		case a == "--flake" && !flake && i+1 < len(argv) && ownFlake.MatchString(argv[i+1]):
			flake = true
			i++
		case strings.HasPrefix(a, "--flake=") && !flake && ownFlake.MatchString(strings.TrimPrefix(a, "--flake=")):
			flake = true
		default:
			return false
		}
	}
	return action && flake
}

// ownFlake matches /etc/nixos or a worker's worktree of it, with a host attribute or without.
var ownFlake = regexp.MustCompile(`^/etc/nixos(/\.claude/worktrees/[a-z0-9][a-z0-9-]*)?/?(#[A-Za-z0-9_-]+)?$`)

// simpleCommands splits a shell script into the argument lists of its simple commands, closely enough to find a git call. It honours quotes and comments, splits at operators, parentheses and backticks, and adds the commands substituted inside double quotes.
func simpleCommands(script string) [][]string {
	var (
		cmds   [][]string
		argv   []string
		word   strings.Builder
		inWord bool
	)
	endWord := func() {
		if inWord {
			argv = append(argv, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCmd := func() {
		endWord()
		if len(argv) > 0 {
			cmds = append(cmds, argv)
			argv = nil
		}
	}
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case c == '\\' && i+1 < len(script):
			i++
			word.WriteByte(script[i])
			inWord = true
		case c == '\'':
			end := strings.IndexByte(script[i+1:], '\'')
			if end < 0 {
				end = len(script) - i - 1
			}
			word.WriteString(script[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			j := i + 1
			for ; j < len(script) && script[j] != '"'; j++ {
				switch {
				case script[j] == '\\' && j+1 < len(script):
					j++
					word.WriteByte(script[j])
				case strings.HasPrefix(script[j:], "$("):
					end := closingParen(script, j+2)
					cmds = append(cmds, simpleCommands(script[j+2:end])...)
					j = end
				case script[j] == '`':
					end := strings.IndexByte(script[j+1:], '`')
					if end < 0 {
						end = len(script) - j - 1
					}
					cmds = append(cmds, simpleCommands(script[j+1:j+1+end])...)
					j += end + 1
				default:
					word.WriteByte(script[j])
				}
			}
			i = j
			inWord = true
		case c == '#' && !inWord:
			for i < len(script) && script[i] != '\n' {
				i++
			}
			endCmd()
		case c == ' ' || c == '\t' || c == '<' || c == '>':
			endWord()
		case strings.IndexByte(";&|\n()`", c) >= 0:
			endCmd()
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endCmd()
	return cmds
}

func closingParen(s string, i int) int {
	for depth := 1; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(s)
}
