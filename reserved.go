package main

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// gate checks a session's tool calls for reserved actions, which wait for approval before they run.
type gate struct {
	// cwd is where the call runs.
	cwd string
	// writable lists where the session's file tools may write, its worktree first, and fenced the folders within them that they may not.
	writable []string
	fenced   []string
	list     userList
	repos    repos
	// unboxed marks a call outside the sandbox, where a push runs the git setup of its repository.
	unboxed bool
}

// repos is what the gate asks of the repository a git push runs in.
type repos interface {
	// remotes lists the remotes configured in dir's repository.
	remotes(dir string) []string
	// changed says how dir's repository differs from bosun's record of its git setup, completing "a repository …", or returns "".
	changed(dir string) string
}

// reservation says why a tool call is a reserved action, and whether only the user may allow it. The zero value is a call that is none.
type reservation struct {
	reason string
	// user marks an action the master may not allow, which goes straight to the user.
	user bool
	// rule is the stable id of the rule that matched, which the log records, and match the word or path it matched.
	rule, match string
}

// The rules' ids. The log keeps them across releases, so each keeps its id as its wording changes.
const (
	rulePushForce   = "push.force"
	rulePushDelete  = "push.delete"
	rulePushEvery   = "push.every"
	rulePushMirror  = "push.mirror"
	rulePushRemote  = "push.remote"
	rulePushOption  = "push.option"
	rulePushSetup   = "push.setup"
	ruleBranch      = "branch.delete"
	ruleWriteDesk   = "write.desk"
	ruleWriteList   = "write.list"
	ruleWriteOut    = "write.outside"
	ruleRoot        = "unboxed.root"
	ruleCron        = "unboxed.cron"
	ruleSteer       = "unboxed.steer"
	ruleSchedule    = "unboxed.schedule"
	ruleProgram     = "unboxed.program"
	ruleUnresolved  = "unboxed.unresolved"
	ruleGitAlias    = "git.alias"
	ruleActivate    = "unboxed.activate"
	ruleUnboxWrite  = "unboxed.write"
	ruleUnboxRead   = "unboxed.read"
	ruleUnboxNames  = "unboxed.names"
	ruleUnboxRunsIn = "unboxed.runsin"
	ruleUnboxed     = "unboxed"
)

// or returns the stronger of r and o: a reservation over none, and the user's over the master's, r when they tie.
func (r reservation) or(o reservation) reservation {
	if r.reason == "" || o.user && !r.user {
		return o
	}
	return r
}

// rebuildRules make every rebuild that the allowlist lets out of the sandbox reach the master, which auto mode would otherwise decide alone.
// Each command rebuild allows must match one of them.
var rebuildRules = []string{"Bash(nixos-rebuild:*)", "Bash(nxt)", "Bash(nxc)"}

// reserved says what in a tool call needs approval before it runs.
func (g gate) reserved(tool string, input json.RawMessage) reservation {
	switch tool {
	case "Bash":
		var in struct {
			Command string `json:"command"`
			Unboxed bool   `json:"dangerouslyDisableSandbox"`
		}
		if json.Unmarshal(input, &in) == nil {
			g.unboxed = in.Unboxed
			if r := g.command(in.Command, []string{g.cwd}); r.user || !in.Unboxed {
				return r
			}
			// Auto mode approves running outside the sandbox without asking.
			return g.outside(in.Command)
		}
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		var in struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		}
		_ = json.Unmarshal(input, &in)
		if p := cmp.Or(in.FilePath, in.NotebookPath); p != "" {
			return g.write(p)
		}
	}
	return reservation{}
}

// write checks a write through a file tool to path, and to where the symlinks on its way lead, which the tool follows.
func (g gate) write(path string) reservation {
	if !filepath.IsAbs(path) {
		path = filepath.Join(g.writable[0], path)
	}
	path = filepath.Clean(path)
	r := g.writeAt(path)
	if real := resolve(path); real != path {
		r = r.or(g.resolved().writeAt(real))
	}
	return r
}

// resolved is g with the symlinks along its folders followed, as the session's own folders may lie behind them too.
func (g gate) resolved() gate {
	h := g
	h.writable, h.fenced = make([]string, len(g.writable)), make([]string, len(g.fenced))
	for i, dir := range g.writable {
		h.writable[i] = resolve(dir)
	}
	for i, dir := range g.fenced {
		h.fenced[i] = resolve(dir)
	}
	return h
}

func (g gate) writeAt(path string) reservation {
	switch {
	case within(g.fenced, path):
		// The desk's settings and hooks run outside any sandbox once the desk starts.
		return reservation{"a write into the desk's directory", true, ruleWriteDesk, path}
	case within(g.writable[1:], path):
		return reservation{}
	}
	worktree := within(g.writable[:1], path)
	if what := g.list.holds(path, worktree); what != "" {
		return reservation{"a write to " + what, true, ruleWriteList, path}
	}
	if worktree {
		return reservation{}
	}
	return reservation{"a write outside the worktree", false, ruleWriteOut, path}
}

// resolve follows the symlinks along path as far as they exist. A write through a dangling link creates its target.
func resolve(path string) string {
	real := "/"
	comps := components(path)
	for hops := 0; len(comps) > 0; {
		next := filepath.Join(real, comps[0])
		comps = comps[1:]
		target, err := os.Readlink(next)
		if err != nil {
			real = next
			continue
		}
		// Linux gives up on a path after 40 links.
		if hops++; hops > 40 {
			return path
		}
		if filepath.IsAbs(target) {
			real = "/"
		}
		comps = append(components(target), comps...)
	}
	return real
}

// components splits a path into its names, none for the root.
func components(path string) []string {
	if path = strings.Trim(path, "/"); path == "" {
		return nil
	}
	return strings.Split(path, "/")
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
func (g gate) command(script string, dirs []string) reservation {
	var r reservation
	for _, cmd := range simpleCommands(script) {
		argv := cmd.argv
		if len(argv) == 0 {
			continue
		}
		if r = r.or(g.argv(argv, dirs)); r.user {
			return r
		}
		if argv[0] == "cd" || argv[0] == "pushd" {
			dirs = append(slices.Clip(dirs), chdir(dirs[len(dirs)-1], argv[1:]))
		}
	}
	return r
}

// argv looks for git anywhere in a command, so that wrappers such as sudo, env or xargs need no rules of their own.
func (g gate) argv(argv, dirs []string) reservation {
	var r reservation
	for i, w := range argv {
		if filepath.Base(w) == "git" {
			r = r.or(g.git(argv[i+1:], dirs))
		}
		if s, _, _, ok := inner(argv, i); ok {
			return r.or(g.command(s, dirs))
		}
	}
	return r
}

// inner returns the script that argv[i] runs, which argv[lo:hi] hold: eval's words, a shell's -c, the -c of script and flock, env's -S and nix-shell's --run.
func inner(argv []string, i int) (script string, lo, hi int, ok bool) {
	// after returns the word that follows one of opts, a short one closing a bundle such as -qc included, or what follows a short one in the same word.
	after := func(opts ...string) (string, int, int, bool) {
		for j := i + 1; j < len(argv); j++ {
			a := argv[j]
			for _, o := range opts {
				bundle := len(o) == 2 && len(a) > 2 && a[1] != '-' && a[0] == '-' && strings.HasSuffix(a, o[1:])
				switch {
				case (a == o || bundle) && j+1 < len(argv):
					return argv[j+1], j + 1, j + 2, true
				case len(o) == 2 && len(a) > 2 && strings.HasPrefix(a, o):
					return a[2:], j, j + 1, true
				}
			}
		}
		return "", 0, 0, false
	}
	switch filepath.Base(argv[i]) {
	case "eval":
		return strings.Join(argv[i+1:], " "), i + 1, len(argv), true
	case "sh", "bash", "zsh", "dash":
		for j := i + 1; j+1 < len(argv); j++ {
			if a := argv[j]; len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'c') {
				return argv[j+1], j + 1, j + 2, true
			}
		}
	case "script", "flock":
		return after("-c", "--command")
	case "env":
		return after("-S", "--split-string")
	case "nix-shell":
		return after("--run", "--command")
	}
	return "", 0, 0, false
}

func (g gate) git(args, dirs []string) reservation {
	// An alias can stand for any subcommand, a force push included.
	for j := 0; j < len(args) && strings.HasPrefix(args[j], "-"); j++ {
		v := ""
		switch a := args[j]; {
		case a == "-C":
			j++
		case (a == "-c" || a == "--config-env") && j+1 < len(args):
			j++
			v = args[j]
		case strings.HasPrefix(a, "--config-env="):
			v = strings.TrimPrefix(a, "--config-env=")
		}
		if strings.HasPrefix(v, "alias.") {
			return reservation{"a git command through an alias bosun cannot check", true, ruleGitAlias, v}
		}
	}
	cd, sub, rest, opt := gitCommand(args)
	switch sub {
	case "push":
		if opt != "" {
			return pushOption.on(opt)
		}
		named, r := pushArgs(rest)
		if r.reason != "" {
			return r
		}
		for _, name := range named {
			for _, dir := range dirs {
				if dir == "" || !slices.Contains(g.repos.remotes(at(dir, cd)), name) {
					return reservation{"a push to a URL or an unknown remote", true, rulePushRemote, name}
				}
			}
		}
		if !g.unboxed {
			break
		}
		for _, dir := range dirs {
			if dir == "" {
				return reservation{"a push outside the sandbox from a repository bosun cannot find", true, rulePushSetup, ""}
			}
			if r := g.repos.changed(at(dir, cd)); r != "" {
				return reservation{"a push outside the sandbox from a repository " + r, true, rulePushSetup, at(dir, cd)}
			}
		}
	case "branch":
		for _, a := range rest {
			// git takes any unambiguous prefix of a long option, such as --del.
			if len(a) > 2 && strings.HasPrefix("--delete", a) || len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a, "dD") {
				return reservation{"deleting a branch", false, ruleBranch, a}
			}
		}
	case "update-ref":
		// --stdin reads commands that may delete any ref.
		if i := slices.IndexFunc(rest, func(a string) bool { return a == "-d" || len(a) > 2 && strings.HasPrefix("--stdin", a) }); i >= 0 {
			return reservation{"deleting a branch", false, ruleBranch, rest[i]}
		}
	}
	return reservation{}
}

// gitCommand splits git's arguments into the directories its -C options name, its subcommand and the subcommand's arguments. opt is git's first other option, any of which could move the repository or its config, or "" when it has none.
func gitCommand(args []string) (cd []string, sub string, rest []string, opt string) {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		a := args[i]
		switch a {
		case "-C":
			if i+1 < len(args) {
				cd = append(cd, args[i+1])
			}
			i++
			continue
		case "-c", "--git-dir", "--work-tree", "--namespace", "--config-env":
			i++
		}
		opt = cmp.Or(opt, a)
	}
	if i >= len(args) {
		return cd, "", nil, opt
	}
	return cd, args[i], args[i+1:], opt
}

// pushFlags are git push's options that take no value and change neither where it pushes nor what it may overwrite.
// git takes any unambiguous prefix of a long option, so one missing here counts as unknown.
var pushFlags = []string{"--set-upstream", "--tags", "--follow-tags", "--dry-run", "--porcelain", "--verbose", "--quiet", "--progress", "--no-progress", "--verify", "--no-verify", "--atomic", "--ipv4", "--ipv6"}

// The pushes only the user may approve, before on names what matched.
var (
	pushForce  = reservation{"a force push", true, rulePushForce, ""}
	pushDelete = reservation{"a push that deletes a ref", true, rulePushDelete, ""}
	pushEvery  = reservation{"a push of every branch", true, rulePushEvery, ""}
	pushMirror = reservation{"a mirror push", true, rulePushMirror, ""}
	pushOption = reservation{"a push with an option bosun does not check", true, rulePushOption, ""}
)

// on is r matched on word.
func (r reservation) on(word string) reservation {
	r.match = word
	return r
}

// pushArgs returns the repositories git push's arguments name, or what in them only the user may approve.
func pushArgs(args []string) (repos []string, r reservation) {
	var pos []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
		case a == "--force", a == "--force-with-lease", strings.HasPrefix(a, "--force-with-lease="), a == "--force-if-includes":
			return nil, pushForce.on(a)
		case a == "--delete", a == "--prune":
			return nil, pushDelete.on(a)
		case a == "--all", a == "--branches":
			return nil, pushEvery.on(a)
		case a == "--mirror":
			return nil, pushMirror.on(a)
		case a == "--repo" && i+1 < len(args):
			i++
			repos = append(repos, args[i])
		case strings.HasPrefix(a, "--repo="):
			repos = append(repos, strings.TrimPrefix(a, "--repo="))
		case a == "--push-option":
			i++
		case strings.HasPrefix(a, "--push-option="), slices.Contains(pushFlags, a):
		case strings.HasPrefix(a, "--"):
			return nil, pushOption.on(a)
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'f':
					return nil, pushForce.on(a)
				case 'd':
					return nil, pushDelete.on(a)
				case 'o':
					// The rest of the word is -o's value, or the next word when nothing follows.
					if j == len(a)-1 {
						i++
					}
					j = len(a)
				case 'u', 'q', 'v', 'n', '4', '6':
				default:
					return nil, pushOption.on(a)
				}
			}
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) == 0 {
		return repos, reservation{}
	}
	for _, ref := range pos[1:] {
		switch {
		case strings.HasPrefix(ref, "+"):
			return nil, pushForce.on(ref)
		case ref == ":" || strings.Contains(ref, "*"):
			return nil, pushEvery.on(ref)
		case strings.HasPrefix(ref, ":"):
			return nil, pushDelete.on(ref)
		}
	}
	return append(repos, pos[0]), reservation{}
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

// names says what on the user's list a word names, relative to dir when the word is, or returns "".
// A word that begins with a variable or a ~ that bosun cannot expand, or that is relative to a dir bosun cannot tell, matches only the names the list holds wherever they lie.
func (g gate) names(word, dir string) string {
	path := word
	switch {
	case word == "":
		return ""
	case filepath.IsAbs(word):
	case dir != "" && word[0] != '$' && word[0] != '~':
		path = filepath.Join(dir, word)
	default:
		return g.list.holds(word, false)
	}
	path = filepath.Clean(path)
	if what := g.nameAt(path); what != "" {
		return what
	}
	// A link, such as one a session made in its scratch folder from inside the sandbox, leads a command to its target.
	if real := resolve(path); real != path {
		return g.resolved().nameAt(real)
	}
	return ""
}

func (g gate) nameAt(path string) string {
	if within(g.writable[1:], path) {
		return ""
	}
	return g.list.holds(path, within(g.writable[:1], path))
}

// jqProgram is the index of jq's program in argv, or -1. jq opens no file its program names.
func jqProgram(argv []string) int {
	if len(argv) == 0 || filepath.Base(argv[0]) != "jq" {
		return -1
	}
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--":
			if i+1 < len(argv) {
				return i + 1
			}
			return -1
		case a == "--from-file", len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'f'):
			// The program comes from a file, and every other argument is an input file.
			return -1
		case a == "--arg", a == "--argjson", a == "--slurpfile", a == "--rawfile":
			i += 2
		case a == "--indent", a == "-L", a == "--library-path":
			i++
		case len(a) > 1 && a[0] == '-':
		default:
			return i
		}
	}
	return -1
}

// rootCommands run what follows them as root or as another user.
var rootCommands = []string{"sudo", "sudoedit", "doas", "pkexec", "su", "run0"}

// activations are what nixos-rebuild runs a configuration's activation scripts as root for.
var activations = []string{"switch", "boot", "test", "dry-activate"}

// asRoot says what in a simple command's words runs as root. A word may come from a script that another word holds, such as ssh's.
// The allowlist has already let through the rebuilds the master may decide.
func asRoot(words []string) reservation {
	activates := reservation{"a command outside the sandbox that activates a NixOS configuration", true, ruleActivate, ""}
	for i, w := range words {
		switch b := filepath.Base(w); {
		case slices.Contains(rootCommands, b):
			return reservation{"a command outside the sandbox that runs " + b, true, ruleRoot, w}
		case w == "--sudo", w == "--use-remote-sudo":
			return reservation{"a command outside the sandbox that runs sudo", true, ruleRoot, w}
		// nxt, nxs and nxb run nixos-rebuild test, switch and boot on /etc/nixos.
		case b == "nxt", b == "nxs", b == "nxb":
			return activates.on(w)
		case b == "nixos-rebuild":
			if j := slices.IndexFunc(words[i+1:], func(a string) bool { return slices.Contains(activations, a) }); j >= 0 {
				return activates.on(w + " " + words[i+1+j])
			}
		}
	}
	return reservation{}
}

// userList is what only the user may let a worker write, or a command outside the sandbox name: what changes code that runs later outside any bosun session's view, and the credentials that reach other machines.
type userList struct {
	home   string
	places []place
	// vars holds the values of the variables a command may name a place by.
	vars map[string]string
	// scratch is the sessions' temporary folders, which hold nothing on the list, with * for the node.
	scratch []string
}

// place is a folder that is on the user's list whole.
type place struct{ dir, what string }

const (
	credentials  = "a credential store"
	shellStartup = "a shell startup file"
	claudeConfig = "Claude Code's configuration"
	gitConfig    = "git hooks or config"
	unitFile     = "a systemd unit or an autostart entry"
	cronTable    = "a cron table"
	bosunState   = "bosun's own state"
)

// newUserList places the user's list in home, Claude Code's config directory claude, bosun's state directory state, and the XDG directories that getenv names.
func newUserList(home, claude, state string, getenv func(string) string) userList {
	l := userList{home: home, vars: map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": claude}, scratch: components(nodeTempDir(state, "*"))}
	if user := getenv("USER"); user != "" {
		l.vars["USER"] = user
	}
	l.places = []place{{claude, claudeConfig}, {"/etc/claude-code", claudeConfig}, {state, bosunState}, {"/etc/systemd", unitFile}, {"/etc/xdg/autostart", unitFile}, {"/etc/crontab", cronTable}, {"/etc/cron.d", cronTable}, {"/var/spool/cron", cronTable}}
	config, data := filepath.Join(home, ".config"), filepath.Join(home, ".local", "share")
	for _, x := range []struct {
		env, dir string
		places   []place
	}{
		{"XDG_CONFIG_HOME", config, []place{{"gh", credentials}, {"glab-cli", credentials}, {"tea", credentials}, {"git", gitConfig}, {"fish", shellStartup}, {"environment.d", shellStartup}, {"systemd", unitFile}, {"autostart", unitFile}}},
		{"XDG_DATA_HOME", data, []place{{"keyrings", credentials}, {"systemd", unitFile}}},
	} {
		dirs := []string{x.dir}
		if d := getenv(x.env); d != "" && d != x.dir {
			dirs = append(dirs, d)
		}
		l.vars[x.env] = dirs[len(dirs)-1]
		for _, dir := range dirs {
			for _, p := range x.places {
				l.places = append(l.places, place{filepath.Join(dir, p.dir), p.what})
			}
		}
	}
	return l
}

var (
	credentialNames = []string{".ssh", ".gnupg", ".password-store", ".aws", ".kube", ".docker", ".netrc", ".git-credentials", ".pki"}
	shellNames      = []string{".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile", ".zshrc", ".zshenv", ".zprofile", ".zlogin", ".zlogout", ".xprofile", ".xinitrc", ".xsession", ".xsessionrc", ".pam_environment"}
	// startNames are what in a worktree's .claude folder the session's next start runs outside the sandbox: hooks, and the settings that name them.
	startNames = []string{"settings.json", "settings.local.json", "hooks"}
	gitNames   = []string{"hooks", "config", "config.worktree"}
)

// holds says what on the list path names, a file or a folder that holds one, or returns "". path may hold globs, and a relative one matches only the names the list holds wherever they lie.
// In the session's own worktree, only what the session's next start runs outside the sandbox counts.
func (l userList) holds(path string, worktree bool) string {
	comps := components(path)
	if filepath.IsAbs(path) && len(comps) >= len(l.scratch) && slices.EqualFunc(l.scratch, comps[:len(l.scratch)], like) {
		return ""
	}
	is := func(c string, names ...string) bool {
		return slices.ContainsFunc(names, func(n string) bool { return like(c, n) })
	}
	for i, c := range comps {
		rest := comps[i+1:]
		if is(c, ".claude") && (len(rest) == 0 || !worktree && rest[0] != "worktrees" || worktree && is(rest[0], startNames...)) {
			return claudeConfig
		}
		if is(c, ".mcp.json") {
			return claudeConfig
		}
		// A worktree's .git is a file that names the repository git works in.
		if is(c, ".git") && (len(rest) == 0 || slices.ContainsFunc(rest, func(r string) bool { return is(r, gitNames...) })) {
			return gitConfig
		}
		if worktree {
			continue
		}
		switch {
		case is(c, credentialNames...):
			return credentials
		case is(c, shellNames...):
			return shellStartup
		case is(c, ".claude.json"):
			return claudeConfig
		case is(c, ".gitconfig"):
			return gitConfig
		case is(c, "systemd") && len(rest) > 0 && is(rest[0], "user", "system"):
			return unitFile
		}
	}
	if worktree || !filepath.IsAbs(path) {
		return ""
	}
	for _, p := range l.places {
		d := components(p.dir)
		if n := min(len(comps), len(d)); !slices.EqualFunc(comps[:n], d[:n], like) {
			continue
		}
		if len(comps) < len(d) {
			return "a folder that holds " + p.what
		}
		return p.what
	}
	return ""
}

// like reports whether a path's component, which may hold globs, matches name. As in bash, only a pattern that begins with a dot matches a name that does.
func like(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") || name[0] == '.' && pattern[0] != '.' {
		return false
	}
	ok, _ := filepath.Match(pattern, name)
	return ok
}

// shellVar matches $NAME and ${NAME}.
var shellVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// words splits a word of a command, run in dir, into the paths it may name. It expands the variables that hold the user's folders, brace lists and a leading ~, and splits at what no path on the list holds, such as the spaces of a script that the word passes on.
func (l userList) words(word, dir string) []string {
	word = shellVar.ReplaceAllStringFunc(word, func(v string) string {
		name := strings.Trim(v, "${}")
		if name == "PWD" && dir != "" {
			return dir
		}
		return cmp.Or(l.vars[name], v)
	})
	var out []string
	for _, e := range braces(strings.ReplaceAll(word, `\`, ""), 256) {
		for _, w := range strings.FieldsFunc(e, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("'\"`=:,;|&<>(){}!", r) }) {
			// A piece of a longer word that leads to the root, such as jq's // or the end of sed's s/x//, is an operator rather than a place.
			if w != e && (filepath.IsAbs(w) && filepath.Clean(w) == "/" || !filepath.IsAbs(w) && dir != "" && filepath.Join(dir, w) == "/") {
				continue
			}
			switch {
			case w == "~" || strings.HasPrefix(w, "~/"):
				w = l.home + w[1:]
			case l.vars["USER"] != "" && (w == "~"+l.vars["USER"] || strings.HasPrefix(w, "~"+l.vars["USER"]+"/")):
				w = l.home + w[1+len(l.vars["USER"]):]
			case (w == "~+" || strings.HasPrefix(w, "~+/")) && dir != "":
				w = dir + w[2:]
			}
			out = append(out, w)
		}
	}
	return out
}

// braces expands the brace lists in s, such as .{bashrc,zshrc}, into at most limit words, past which it adds s as it is.
func braces(s string, limit int) []string {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' || i > 0 && s[i-1] == '$' {
			continue
		}
		var alts []string
		start, end := i+1, -1
		for j, depth := i+1, 1; j < len(s) && end < 0; j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				if depth--; depth == 0 {
					end = j
				}
			case ',':
				if depth == 1 {
					alts = append(alts, s[start:j])
					start = j + 1
				}
			}
		}
		if end < 0 || len(alts) == 0 {
			continue
		}
		var out []string
		for _, a := range append(alts, s[start:end]) {
			for _, e := range braces(s[:i]+a+s[end+1:], limit) {
				if len(out) == limit {
					return append(out, s)
				}
				out = append(out, e)
			}
		}
		return out
	}
	return []string{s}
}

// cd is where cd with args leads from dir, or "" when bosun cannot tell.
func (l userList) cd(dir string, args []string) string {
	if len(args) == 0 {
		return l.home
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return ""
	}
	words := l.words(args[0], dir)
	if len(words) != 1 || strings.ContainsAny(words[0], "$~*?[") {
		return ""
	}
	switch w := words[0]; {
	case filepath.IsAbs(w):
		return filepath.Clean(w)
	case dir == "":
		return ""
	default:
		return filepath.Join(dir, w)
	}
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

// simple is a simple command: its arguments, and the files its redirections name.
type simple struct {
	argv  []string
	files []redirect
}

// redirect is the word a redirection names, which > writes and < reads.
type redirect struct {
	word   string
	access access
}

// substituted stands in a word for a command substitution, whose output bosun cannot know, so that the word stays unresolved.
const substituted = "$()"

// simpleCommands splits a shell script into its simple commands, closely enough to find a git call. It honours quotes and comments, splits at operators, parentheses and backticks, and adds the commands substituted inside double quotes.
func simpleCommands(script string) []simple {
	var (
		cmds   []simple
		cmd    simple
		word   strings.Builder
		inWord bool
		// file marks the next word as what a redirection names, and fileAccess what the redirection does with it.
		file       bool
		fileAccess access
	)
	endWord := func() {
		if inWord {
			if file {
				cmd.files = append(cmd.files, redirect{word.String(), fileAccess})
			} else {
				cmd.argv = append(cmd.argv, word.String())
			}
			word.Reset()
			inWord, file = false, false
		}
	}
	endCmd := func() {
		endWord()
		if len(cmd.argv) > 0 || len(cmd.files) > 0 {
			cmds = append(cmds, cmd)
			cmd = simple{}
		}
		file = false
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
					word.WriteString(substituted)
					j = end
				case script[j] == '`':
					end := strings.IndexByte(script[j+1:], '`')
					if end < 0 {
						end = len(script) - j - 1
					}
					cmds = append(cmds, simpleCommands(script[j+1:j+1+end])...)
					word.WriteString(substituted)
					j += end + 1
				default:
					word.WriteByte(script[j])
				}
			}
			i = j
			inWord = true
		case strings.HasPrefix(script[i:], "$("):
			end := closingParen(script, i+2)
			cmds = append(cmds, simpleCommands(script[i+2:end])...)
			word.WriteString(substituted)
			i = end
			inWord = true
		case strings.HasPrefix(script[i:], "$'"):
			// ANSI-C quoting. An escape such as \x2f may spell any byte, so a word that holds one stays unresolved.
			end := i + 2
			for end < len(script) && script[end] != '\'' {
				if script[end] == '\\' {
					end++
				}
				end++
			}
			s := script[i+2 : min(end, len(script))]
			if strings.ContainsRune(s, '\\') {
				s = "$" + s
			}
			word.WriteString(s)
			i = end
			inWord = true
		case c == '#' && !inWord:
			for i < len(script) && script[i] != '\n' {
				i++
			}
			endCmd()
		case c == '<' || c == '>' || c == '&' && strings.HasPrefix(script[i+1:], ">"):
			// The descriptor number before a redirection, as in 2>&1, is no argument.
			if s := word.String(); inWord && s != "" && strings.Trim(s, "0123456789") == "" {
				word.Reset()
				inWord = false
			}
			endWord()
			start := i
			i, file = redirection(script, i)
			fileAccess = reads
			if strings.ContainsRune(script[start:i+1], '>') {
				fileAccess = writes
			}
		case c == ' ' || c == '\t':
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

// redirection reads the redirection operator at script[i]. It returns the index of the operator's last byte, and whether a word naming a file follows, which a descriptor that >&2 duplicates or >&- closes is not.
func redirection(script string, i int) (int, bool) {
	j := i + 1
	if script[i] == '&' {
		j++
	}
	for j < len(script) && (script[j] == '<' || script[j] == '>') {
		j++
	}
	switch {
	// zsh's >! overrides noclobber, as bash's >| does.
	case j < len(script) && (script[j] == '|' || script[j] == '!' || script[j] == '-'):
		j++
	case j < len(script) && script[j] == '&':
		k := j + 1
		for k < len(script) && script[k] >= '0' && script[k] <= '9' {
			k++
		}
		if k < len(script) && script[k] == '-' {
			k++
		}
		if k > j+1 && (k == len(script) || strings.IndexByte(" \t\n;&|()<>", script[k]) >= 0) {
			return k - 1, false
		}
		j++
	}
	return j - 1, true
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
