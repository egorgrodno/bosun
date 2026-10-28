package main

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// outside checks a command outside the sandbox that command found nothing for the user in, so whose pushes come from repositories as bosun recorded them.
// Only a whole command of plain words on the allowlist takes the normal flow: a rebuild that adds no boot entry, or a push.
// Of the rest, the user keeps what runs as root or names a path on the user's list, and the master gets everything else.
func (g gate) outside(script string) reservation {
	argv := strings.Split(script, " ")
	if rebuild(argv) {
		return reservation{}
	}
	if allPlain(argv) && argv[0] == "git" {
		if _, sub, _, opt := gitCommand(argv[1:]); sub == "push" && opt == "" {
			return reservation{}
		}
	}
	if r := g.userUnboxed(script, g.cwd); r.reason != "" {
		return r
	}
	r := reservation{"a command outside the sandbox", false, ruleUnboxed, ""}
	if cmds := simpleCommands(script); len(cmds) > 0 && len(cmds[0].argv) > 0 {
		r.match = cmds[0].argv[0]
	}
	return r
}

// userUnboxed says what in script, run outside the sandbox in dir, only the user may allow. dir is "" when bosun cannot tell where a command runs.
// A command there can write anywhere, so this catches only what a word names: a path on the user's list that the command may write, wherever in the word, a credential it reads, or a command that runs as root, steers bosun or schedules a job.
// It trusts that a path is only read when it understands every command of the script. One command it does not, such as xargs fed by a pipe, may write any path the others name.
func (g gate) userUnboxed(script, dir string) reservation {
	cmds := simpleCommands(script)
	names, all, sets := rebinds(cmds)
	g.list = g.list.unbound(names, all)
	// A variable set in the environment can make a reader run a command, as RIPGREP_CONFIG_PATH or LD_PRELOAD does.
	sure := !sets && !slices.ContainsFunc(cmds, func(c simple) bool { return !g.list.understood(c.argv) })
	for _, cmd := range cmds {
		argv := cmd.argv
		var words []string
		expanded := make([][]string, len(argv))
		for i, w := range argv {
			expanded[i] = g.list.words(w, dir)
			words = append(words, expanded[i]...)
		}
		// A rebuild the allowlist takes stays the master's beside other commands, which this checks apart.
		if r := asRoot(words); r.reason != "" && !rebuild(argv) {
			return r
		}
		for _, check := range []func([]string, bool) reservation{steers, schedules, unknownProgram, gitConfigWrite} {
			if r := check(argv, sure); r.reason != "" {
				return r
			}
		}
		acc, reader := accesses(argv)
		if !sure {
			acc, reader = conservative(argv, acc, g.list.understood(argv)), false
			// A cd bosun can follow writes nothing, and what runs after it counts where it leads. One it cannot follow, such as cd -P, leaves later paths unplaced, so its own counts.
			if len(argv) > 1 && (argv[0] == "cd" || argv[0] == "pushd") && g.list.cd(dir, argv[1:]) != "" {
				acc[1] = reads
			}
		}
		var scripts []string
		for i := range argv {
			if s, lo, hi, ok := inner(argv, i); ok {
				scripts = append(scripts, s)
				for j := lo; j < hi && sure; j++ {
					acc[j] = text
				}
			}
		}
		for i := range argv {
			if r := g.uses(expanded[i], dir, acc[i]); r.reason != "" {
				return r
			}
		}
		for _, f := range cmd.files {
			a := f.access
			if !sure && a == reads {
				a = unclear
			}
			if r := g.uses(g.list.words(f.word, dir), dir, a); r.reason != "" {
				return r
			}
		}
		for _, s := range scripts {
			if r := g.userUnboxed(s, dir); r.reason != "" {
				return r
			}
		}
		// A command that writes may write where it runs without naming the place.
		if what := g.names(dir, ""); what != "" && (!reader || what == credentials) {
			return reservation{"a command outside the sandbox that runs in " + what, true, ruleUnboxRunsIn, dir}
		}
		if len(argv) > 0 && (argv[0] == "cd" || argv[0] == "pushd") {
			dir = g.list.cd(dir, argv[1:])
		}
	}
	return reservation{}
}

// setters are the builtins that set the variables their arguments name, and nameref marks the option that makes one variable stand for another.
var setters = []string{"export", "declare", "typeset", "local", "readonly", "let", "unset", "read", "mapfile", "readarray", "getopts", "vared", "for", "select"}

// rebinds says which variables cmds, and the scripts they run, may set or clear: by an assignment, a builtin such as export, read or printf -v, env -u, or a for loop.
// all marks a command that may set or clear any, such as source, a nameref or env -i, and sets one that sets a variable at all.
func rebinds(cmds []simple) (names []string, all, sets bool) {
	for _, c := range cmds {
		argv := c.argv
		i := 0
		for ; i < len(argv) && assignment.MatchString(argv[i]); i++ {
			names, sets = append(names, varName(argv[i])), true
		}
		if i == len(argv) {
			continue
		}
		args := argv[i+1:]
		switch name := filepath.Base(argv[i]); {
		case name == "source", name == ".":
			return nil, true, true
		case name == "env":
			// env's own options stop at its program.
			opts := true
			for j := 0; j < len(args); j++ {
				switch a := args[j]; {
				case assignment.MatchString(a):
					names, sets = append(names, varName(a)), true
				case !opts:
				case a == "-i", a == "--ignore-environment", a == "-":
					return nil, true, true
				case (a == "-u" || a == "--unset") && j+1 < len(args):
					j++
					names, sets = append(names, args[j]), true
				case strings.HasPrefix(a, "--unset="), strings.HasPrefix(a, "-u"):
					names, sets = append(names, strings.TrimPrefix(strings.TrimPrefix(a, "--unset="), "-u")), true
				case !strings.HasPrefix(a, "-"):
					opts = false
				}
			}
		case name == "printf":
			if j := slices.Index(args, "-v"); j >= 0 && j+1 < len(args) {
				names, sets = append(names, args[j+1]), true
			}
		case slices.Contains(setters, name):
			sets = true
			for _, a := range args {
				switch {
				case strings.HasPrefix(a, "-") && strings.ContainsRune(a, 'n') && name != "read" && name != "unset":
					return nil, true, true
				case !strings.HasPrefix(a, "-"):
					names = append(names, varName(a))
				}
			}
		}
		for j := range argv {
			if s, _, _, ok := inner(argv, j); ok {
				n, a, set := rebinds(simpleCommands(s))
				if a {
					return nil, true, true
				}
				names, sets = append(names, n...), sets || set
			}
		}
	}
	return names, false, sets
}

// varName is the variable an assignment or a builtin's argument names, without its value or index.
func varName(a string) string {
	if i := strings.IndexAny(a, "+=["); i >= 0 {
		return a[:i]
	}
	return a
}

// unbound is l without the values of the variables in names, or of every variable when all is set, for a script that may set them. ~ follows HOME.
func (l userList) unbound(names []string, all bool) userList {
	moves := []string{"PWD", "OLDPWD", "CDPATH"}
	if !all && !slices.ContainsFunc(names, func(n string) bool { _, ok := l.vars[n]; return ok || slices.Contains(moves, n) }) {
		return l
	}
	vars := map[string]string{}
	for k, v := range l.vars {
		if !all && !slices.Contains(names, k) {
			vars[k] = v
		}
	}
	l.vars = vars
	l.moved = l.moved || all || slices.ContainsFunc(names, func(n string) bool { return slices.Contains(moves, n) })
	return l
}

// uses says whether a word that expands to paths, used as a in a command run in dir, names a path on the user's list that only the user may let it use: any it may write, and a credential it reads.
func (g gate) uses(paths []string, dir string, a access) reservation {
	if a == text {
		return reservation{}
	}
	for _, p := range paths {
		what := g.names(p, dir)
		switch {
		case what != "" && (a != reads || what == credentials):
			return reservation{"a command outside the sandbox that names " + what, true, accessRules[a], p}
		case what == "" && a != reads && g.couldLand(p, dir, a == writes):
			return reservation{"a command outside the sandbox that may write a path bosun cannot resolve", true, ruleUnresolved, p}
		}
	}
	return reservation{}
}

// couldLand reports whether path p, which holds a variable, a substitution or a ~ that bosun cannot expand, may lead to the user's list: through what comes before the unknown part, or after it.
// A path that begins with the unknown part may lead anywhere, so for a sure write, which sure is, it counts whatever follows.
func (g gate) couldLand(p, dir string, sure bool) bool {
	// A relative path after a cd bosun could not follow lies anywhere, and so may what zsh's ** matches, through links at any depth.
	if sure && (dir == "" && !filepath.IsAbs(p) || strings.Contains(p, "**")) {
		return true
	}
	i := strings.IndexAny(p, "$`")
	if strings.HasPrefix(p, "~") {
		i = 0
	}
	if i < 0 {
		return false
	}
	before := p[:strings.LastIndexByte(p[:i], '/')+1]
	after := ""
	if j := strings.IndexByte(p[i:], '/'); j >= 0 {
		after = strings.Trim(p[i+j:], "/")
	}
	switch {
	case before != "":
		return g.names(before, dir) != ""
	case sure:
		return true
	case after == "":
		return false
	case g.list.holds(after, false) != "":
		return true
	}
	return slices.ContainsFunc(g.list.places, func(pl place) bool {
		rel := strings.TrimPrefix(strings.TrimPrefix(pl.dir, g.list.home), "/")
		return after == rel || strings.HasPrefix(after, rel+"/") || strings.HasPrefix(rel, after+"/")
	})
}

// access is what a command does with the path a word of it names.
type access int

const (
	// unclear is a word of a command bosun does not know, which may write what it names.
	unclear access = iota
	reads
	writes
	// text names no path, such as jq's program or a shell's script, which bosun checks apart.
	text
)

// accessRules are the rules that a path on the user's list meets, by what the command does with it.
var accessRules = [...]string{unclear: ruleUnboxNames, reads: ruleUnboxRead, writes: ruleUnboxWrite}

var (
	// readers write no file their arguments name, and run no command.
	readers = []string{"cat", "head", "tail", "grep", "egrep", "fgrep", "rg", "ls", "wc", "stat", "du", "df", "diff", "cmp", "md5sum", "sha1sum", "sha256sum", "sha512sum", "b2sum", "cksum", "realpath", "readlink", "basename", "dirname", "test", "[", "pwd", "cd", "pushd", "column", "cut", "nl", "od", "hexdump", "strings"}
	// texters take text, and open no file their arguments name.
	texters = []string{"echo", "printf", "true", "false", ":"}
	// findActions make find write or run a command.
	findActions = []string{"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls"}
	// writers may write every file their arguments name. Both of ln's do, since writing through the link writes its target.
	writers = []string{"rm", "rmdir", "unlink", "shred", "truncate", "touch", "mkdir", "chmod", "chown", "chgrp", "chattr", "setfacl", "tee", "mv", "ln"}
	// copiers write their last argument and read the rest, given only the options in copierOptions.
	copiers = []string{"cp", "install", "rsync", "scp"}
	// assignment matches a variable's assignment before a command.
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

// options are a command's options that bosun knows: short flags as letters, and long ones whole.
type options struct {
	short string
	long  []string
}

// copierOptions are the options under which a copier writes only its last argument and keeps its sources. cp's -l and -s link instead of copying, and -t, -S and the like take a value.
var copierOptions = map[string]options{
	"cp":      {"aRrfinpvLPHuxdTb", []string{"--recursive", "--force", "--interactive", "--no-clobber", "--verbose", "--archive", "--preserve", "--no-dereference", "--dereference", "--update", "--one-file-system", "--no-target-directory"}},
	"install": {"Dcpv", []string{"--compare", "--preserve-timestamps", "--verbose"}},
	"rsync":   {"avzrlptgoDhPnqcuHSWx", []string{"--archive", "--verbose", "--compress", "--recursive", "--progress", "--dry-run", "--human-readable", "--checksum", "--update", "--partial", "--times", "--perms", "--links", "--stats", "--itemize-changes"}},
	"scp":     {"rpqvCB346", nil},
}

// plain reports whether args hold only options that o knows.
func (o options) plain(args []string) bool {
	for _, a := range args {
		switch {
		case a == "--":
			return true
		case strings.HasPrefix(a, "--"):
			if !slices.Contains(o.long, a) {
				return false
			}
		case len(a) > 1 && a[0] == '-':
			if strings.Trim(a[1:], o.short) != "" {
				return false
			}
		}
	}
	return true
}

// wrapper is what bosun knows of a command that runs the command after its options: the options with no value, and those that take one.
type wrapper struct{ flags, values []string }

// wrappers run the command that follows their options. timeout's duration and env's assignments come first.
var wrappers = map[string]wrapper{
	"env":     {[]string{"-i", "--ignore-environment", "-0", "--null"}, []string{"-u", "--unset"}},
	"command": {[]string{"-p"}, nil},
	"exec":    {},
	"nohup":   {},
	"time":    {},
	"nice":    {nil, []string{"-n", "--adjustment"}},
	"timeout": {[]string{"--preserve-status", "--foreground", "-v", "--verbose"}, []string{"-s", "--signal", "-k", "--kill-after"}},
	"stdbuf":  {nil, []string{"-i", "-o", "-e", "--input", "--output", "--error"}},
	"setsid":  {[]string{"-f", "--fork", "-w", "--wait", "-c", "--ctty"}, nil},
	"ionice":  {[]string{"-t", "--ignore"}, []string{"-c", "--class", "-n", "--classdata"}},
}

// commandAt is the index of the program argv runs, past the assignments and wrappers before it, or len(argv) when it runs none.
// ok is false when a wrapper has an option bosun does not know, such as env -C, which moves where the command runs.
func commandAt(argv []string) (i int, ok bool) {
	for i < len(argv) && assignment.MatchString(argv[i]) {
		i++
	}
	for i < len(argv) {
		name := filepath.Base(argv[i])
		w, known := wrappers[name]
		if !known {
			return i, true
		}
		for i++; i < len(argv) && len(argv[i]) > 1 && argv[i][0] == '-'; i++ {
			a := argv[i]
			if a == "--" {
				i++
				break
			}
			value := slices.ContainsFunc(w.values, func(v string) bool {
				return a != v && (strings.HasPrefix(a, v+"=") || len(v) == 2 && strings.HasPrefix(a, v))
			})
			switch {
			case slices.Contains(w.values, a):
				i++
			case value, slices.Contains(w.flags, a), name == "nice" && strings.Trim(a[1:], "0123456789") == "":
			default:
				return i, false
			}
		}
		for name == "env" && i < len(argv) && assignment.MatchString(argv[i]) {
			i++
		}
		if name == "timeout" && i < len(argv) {
			i++
		}
	}
	return i, true
}

// understood reports whether bosun knows all that argv may do with the paths it names.
func (l userList) understood(argv []string) bool {
	i, ok := commandAt(argv)
	if !ok {
		return false
	}
	if i == len(argv) {
		return true
	}
	name, args := filepath.Base(argv[i]), argv[i+1:]
	switch {
	case strings.ContainsAny(argv[i], "$`"):
		return false
	case name == "cd", name == "pushd":
		return len(args) == 0 || l.cd("/", args) != ""
	case name == "find":
		return !slices.ContainsFunc(args, func(a string) bool { return slices.Contains(findActions, a) })
	case name == "rg":
		// --pre and --hostname-bin run a command.
		return !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--pre") || strings.HasPrefix(a, "--hostname-bin") })
	case slices.Contains(copiers, name):
		return copierOptions[name].plain(args)
	}
	return slices.Contains(readers, name) || slices.Contains(texters, name) || slices.Contains(writers, name) || name == "jq" || name == "dd" || name == "bosun"
}

// programs are the indexes in argv of what may run: the program, when bosun understands the command, and otherwise any word, as xargs and find -exec run one.
func programs(argv []string, sure bool) []int {
	if sure {
		if i, _ := commandAt(argv); i < len(argv) {
			return []int{i}
		}
		return nil
	}
	var all []int
	for i := range argv {
		all = append(all, i)
	}
	return all
}

// bosunReads are the bosun subcommands that only read. The empty one prints the usage.
var bosunReads = []string{"", "status", "conversation", "log", "follow", "watch", "help", "-h", "-help", "--help"}

// steers says whether argv runs a bosun subcommand that acts as the user, which the daemon cannot tell from the user's own frontend, or one bosun does not know.
func steers(argv []string, sure bool) reservation {
	for _, i := range programs(argv, sure) {
		if filepath.Base(argv[i]) != "bosun" {
			continue
		}
		sub := ""
		if i+1 < len(argv) {
			sub = argv[i+1]
		}
		if !slices.Contains(bosunReads, sub) {
			return reservation{"a command outside the sandbox that steers bosun as the user", true, ruleSteer, strings.TrimSpace("bosun " + sub)}
		}
	}
	return reservation{}
}

// unitVerbs are what systemctl changes a unit's setup with, rather than starting or reading it.
var unitVerbs = []string{"enable", "reenable", "link", "edit", "preset", "preset-all", "set-property", "add-wants", "add-requires", "mask", "unmask", "revert", "set-default", "set-environment", "import-environment"}

// schedules says whether argv sets up code to run later: a cron table, an at job, a transient unit or a change to a unit.
func schedules(argv []string, sure bool) reservation {
	for _, i := range programs(argv, sure) {
		name, rest := filepath.Base(argv[i]), argv[i+1:]
		switch {
		// crontab is setuid, and installs a table that cron runs later. Only -l reads one.
		case name == "crontab" && !slices.Contains(rest, "-l"):
			return reservation{"a command outside the sandbox that edits " + cronTable, true, ruleCron, argv[i]}
		case (name == "at" || name == "batch") && !slices.Contains(rest, "-l"), name == "systemd-run",
			name == "systemctl" && slices.ContainsFunc(rest, func(a string) bool { return slices.Contains(unitVerbs, a) }):
			return reservation{"a command outside the sandbox that schedules a job or changes a unit", true, ruleSchedule, argv[i]}
		}
	}
	return reservation{}
}

// unknownProgram says whether argv runs a program that a variable or a substitution names, which could be bosun or anything else.
func unknownProgram(argv []string, _ bool) reservation {
	i, _ := commandAt(argv)
	if i < len(argv) && strings.HasPrefix(argv[i], "$") && !strings.Contains(argv[i], "/") {
		return reservation{"a command outside the sandbox whose program bosun cannot tell", true, ruleProgram, argv[i]}
	}
	return reservation{}
}

// gitConfigReads are the options and subcommands with which git config only reads.
var gitConfigReads = []string{"--get", "--get-all", "--get-regexp", "--get-urlmatch", "--get-color", "--get-colorbool", "--list", "-l", "get", "list"}

// gitConfigWrite says whether argv writes the user's git config with git config --global or --system, which names no file.
func gitConfigWrite(argv []string, _ bool) reservation {
	for i, w := range argv {
		if filepath.Base(w) != "git" {
			continue
		}
		_, sub, rest, _ := gitCommand(argv[i+1:])
		if sub != "config" || slices.ContainsFunc(rest, func(a string) bool { return slices.Contains(gitConfigReads, a) }) {
			continue
		}
		if j := slices.IndexFunc(rest, func(a string) bool { return a == "--global" || a == "--system" }); j >= 0 {
			return reservation{"a command outside the sandbox that names " + gitConfig, true, ruleUnboxWrite, rest[j]}
		}
	}
	return reservation{}
}

// accesses says what argv, which bosun understands, does with the path each of its words may name, and whether it writes none of them.
func accesses(argv []string) (acc []access, reader bool) {
	acc = make([]access, len(argv))
	i, _ := commandAt(argv)
	if i == len(argv) {
		return acc, true
	}
	program(acc, argv, i)
	args := acc[i+1:]
	set := func(a access) {
		for j := range args {
			args[j] = a
		}
	}
	switch name := filepath.Base(argv[i]); {
	// bosun's reading commands take words, not paths. steers holds the rest for the user.
	case slices.Contains(texters, name), name == "bosun" && (i+1 == len(argv) || slices.Contains(bosunReads, argv[i+1])):
		set(text)
		return acc, true
	case name == "jq":
		set(reads)
		jqText(acc, argv, i)
		return acc, true
	case slices.Contains(readers, name), name == "find":
		set(reads)
		return acc, true
	case slices.Contains(writers, name):
		set(writes)
	case name == "dd":
		for j, a := range argv[i+1:] {
			args[j] = reads
			if strings.HasPrefix(a, "of=") {
				args[j] = writes
			}
		}
	case slices.Contains(copiers, name):
		set(reads)
		last := -1
		for j := i + 1; j < len(argv); j++ {
			if !strings.HasPrefix(argv[j], "-") || argv[j] == "-" {
				last = j
			}
		}
		if last >= 0 {
			acc[last] = writes
		}
	}
	return acc, false
}

// conservative is what argv, in a script bosun does not understand, may do with each word: write any path it names, though running a program reads it, jq's program and the words of bosun's reading commands name none.
// A command that bosun understands keeps the writes that accesses found in sorted.
func conservative(argv []string, sorted []access, understood bool) []access {
	acc := make([]access, len(argv))
	for j := range acc {
		if understood && sorted[j] == writes {
			acc[j] = writes
		}
	}
	i, _ := commandAt(argv)
	if i == len(argv) {
		return acc
	}
	program(acc, argv, i)
	switch filepath.Base(argv[i]) {
	case "jq":
		jqText(acc, argv, i)
	case "bosun":
		if i+1 == len(argv) || slices.Contains(bosunReads, argv[i+1]) {
			for j := i + 1; j < len(argv); j++ {
				acc[j] = text
			}
		}
	}
	return acc
}

// program marks argv[i], the program, as read when it names a path. A bare name runs one from PATH.
func program(acc []access, argv []string, i int) {
	acc[i] = text
	if strings.ContainsRune(argv[i], '/') {
		acc[i] = reads
	}
}

// jqText marks the program of the jq at argv[i] as text, unless it imports a module, which it reads from a path.
func jqText(acc []access, argv []string, i int) {
	if p := jqProgram(argv[i:]); p >= 0 && !strings.Contains(argv[i+p], "import") && !strings.Contains(argv[i+p], "include") {
		acc[i+p] = text
	}
}
