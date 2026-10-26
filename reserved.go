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
}

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
		// The session's own folders may lie behind symlinks too.
		h := g
		h.writable, h.fenced = make([]string, len(g.writable)), make([]string, len(g.fenced))
		for i, dir := range g.writable {
			h.writable[i] = resolve(dir)
		}
		for i, dir := range g.fenced {
			h.fenced[i] = resolve(dir)
		}
		r = r.or(h.writeAt(real))
	}
	return r
}

func (g gate) writeAt(path string) reservation {
	switch {
	case within(g.fenced, path):
		// The desk's settings and hooks run outside any sandbox once the desk starts.
		return reservation{"a write into the desk's directory", true}
	case within(g.writable[1:], path):
		return reservation{}
	}
	worktree := within(g.writable[:1], path)
	if what := g.list.holds(path, worktree); what != "" {
		return reservation{"a write to " + what, true}
	}
	if worktree {
		return reservation{}
	}
	return reservation{reason: "a write outside the worktree"}
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
	for _, argv := range simpleCommands(script) {
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
		if s, ok := inner(argv, i); ok {
			return r.or(g.command(s, dirs))
		}
	}
	return r
}

// inner returns the script that argv[i] runs when it is eval or a shell's -c.
func inner(argv []string, i int) (string, bool) {
	switch filepath.Base(argv[i]) {
	case "eval":
		return strings.Join(argv[i+1:], " "), true
	case "sh", "bash", "zsh", "dash":
		for j := i + 1; j+1 < len(argv); j++ {
			if a := argv[j]; len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'c') {
				return argv[j+1], true
			}
		}
	}
	return "", false
}

func (g gate) git(args, dirs []string) reservation {
	cd, sub, rest, plain := gitCommand(args)
	switch sub {
	case "push":
		if !plain {
			return reservation{"a push with an option bosun does not check", true}
		}
		named, r := pushArgs(rest)
		if r != "" {
			return reservation{r, true}
		}
		for _, name := range named {
			for _, dir := range dirs {
				if dir == "" || !slices.Contains(g.repos.remotes(at(dir, cd)), name) {
					return reservation{"a push to a URL or an unknown remote", true}
				}
			}
		}
		if !g.unboxed {
			break
		}
		for _, dir := range dirs {
			if dir == "" {
				return reservation{"a push outside the sandbox from a repository bosun cannot find", true}
			}
			if r := g.repos.changed(at(dir, cd)); r != "" {
				return reservation{"a push outside the sandbox from a repository " + r, true}
			}
		}
	case "branch":
		for _, a := range rest {
			// git takes any unambiguous prefix of a long option, such as --del.
			if len(a) > 2 && strings.HasPrefix("--delete", a) || len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a, "dD") {
				return reservation{reason: "deleting a branch"}
			}
		}
	case "update-ref":
		// --stdin reads commands that may delete any ref.
		if slices.Contains(rest, "-d") || slices.ContainsFunc(rest, func(a string) bool { return len(a) > 2 && strings.HasPrefix("--stdin", a) }) {
			return reservation{reason: "deleting a branch"}
		}
	}
	return reservation{}
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

// outside checks a command outside the sandbox that command found nothing for the user in, so whose pushes come from repositories as bosun recorded them.
// Only a whole command of plain words on the allowlist takes the normal flow: a rebuild that adds no boot entry, or a push.
// Of the rest, the user keeps what runs as root or names a path on the user's list, and the master gets everything else.
func (g gate) outside(script string) reservation {
	argv := strings.Split(script, " ")
	if rebuild(argv) {
		return reservation{}
	}
	if !slices.ContainsFunc(argv, func(w string) bool { return !plainWord.MatchString(w) }) && argv[0] == "git" {
		if _, sub, _, plain := gitCommand(argv[1:]); sub == "push" && plain {
			return reservation{}
		}
	}
	if what := g.names(g.cwd, ""); what != "" {
		return reservation{"a command outside the sandbox that runs in " + what, true}
	}
	if r := g.userUnboxed(script, g.cwd); r != "" {
		return reservation{r, true}
	}
	return reservation{reason: "a command outside the sandbox"}
}

// userUnboxed names what in script, run outside the sandbox in dir, only the user may allow, or returns "". dir is "" when bosun cannot tell where a command runs.
// A command there can write anywhere, so this catches only what a word names: a path on the user's list, wherever in the word, or a command that runs as root.
func (g gate) userUnboxed(script, dir string) string {
	for _, argv := range simpleCommands(script) {
		var words []string
		for _, w := range argv {
			words = append(words, g.list.words(w, dir)...)
		}
		if r := asRoot(words); r != "" {
			return r
		}
		for _, w := range words {
			if what := g.names(w, dir); what != "" {
				return "a command outside the sandbox that names " + what
			}
		}
		for i := range argv {
			if s, ok := inner(argv, i); ok {
				if r := g.userUnboxed(s, dir); r != "" {
					return r
				}
			}
		}
		if argv[0] == "cd" || argv[0] == "pushd" {
			dir = g.list.cd(dir, argv[1:])
			if what := g.names(dir, ""); what != "" {
				return "a command outside the sandbox that runs in " + what
			}
		}
	}
	return ""
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
	if within(g.writable[1:], path) {
		return ""
	}
	return g.list.holds(path, within(g.writable[:1], path))
}

// rootCommands run what follows them as root or as another user.
var rootCommands = []string{"sudo", "sudoedit", "doas", "pkexec", "su", "run0"}

// activations are what nixos-rebuild runs a configuration's activation scripts as root for.
var activations = []string{"switch", "boot", "test", "dry-activate"}

// asRoot names what in a simple command's words runs as root, or returns "". A word may come from a script that another word holds, such as ssh's.
// The allowlist has already let through the rebuilds the master may decide.
func asRoot(words []string) string {
	const activates = "a command outside the sandbox that activates a NixOS configuration"
	for i, w := range words {
		switch b := filepath.Base(w); {
		case slices.Contains(rootCommands, b):
			return "a command outside the sandbox that runs " + b
		case w == "--sudo", w == "--use-remote-sudo":
			return "a command outside the sandbox that runs sudo"
		// nxt, nxs and nxb run nixos-rebuild test, switch and boot on /etc/nixos.
		case b == "nxt", b == "nxs", b == "nxb":
			return activates
		case b == "nixos-rebuild" && slices.ContainsFunc(words[i+1:], func(a string) bool { return slices.Contains(activations, a) }):
			return activates
		}
	}
	return ""
}

// userList is what only the user may let a worker write, or a command outside the sandbox name: what changes code that runs later outside any bosun session's view, and the credentials that reach other machines.
type userList struct {
	home   string
	places []place
	// vars holds the values of the variables a command may name a place by.
	vars map[string]string
}

// place is a folder that is on the user's list whole.
type place struct{ dir, what string }

const (
	credentials  = "a credential store"
	shellStartup = "a shell startup file"
	claudeConfig = "Claude Code's configuration"
	gitConfig    = "git hooks or config"
	unitFile     = "a systemd unit or an autostart entry"
	bosunState   = "bosun's own state"
)

// newUserList places the user's list in home, Claude Code's config directory claude, bosun's state directory state, and the XDG directories that getenv names.
func newUserList(home, claude, state string, getenv func(string) string) userList {
	l := userList{home: home, vars: map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": claude}}
	if user := getenv("USER"); user != "" {
		l.vars["USER"] = user
	}
	l.places = []place{{claude, claudeConfig}, {"/etc/claude-code", claudeConfig}, {state, bosunState}, {"/etc/systemd", unitFile}, {"/etc/xdg/autostart", unitFile}}
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
			switch {
			case w == "~" || strings.HasPrefix(w, "~/"):
				w = l.home + w[1:]
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
