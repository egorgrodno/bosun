package main

import (
	"cmp"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
)

// reservedAction names what in a tool call only the user may approve, or returns "". A file tool may write within dirs, the first of which is the session's worktree.
func reservedAction(tool string, input json.RawMessage, dirs ...string) string {
	switch tool {
	case "Bash":
		var in struct {
			Command string `json:"command"`
			Unboxed bool   `json:"dangerouslyDisableSandbox"`
		}
		if json.Unmarshal(input, &in) == nil {
			if r := reservedCommand(in.Command); r != "" || !in.Unboxed {
				return r
			}
			// Auto mode approves running outside the sandbox without asking.
			return "a command outside the sandbox"
		}
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		var in struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		}
		_ = json.Unmarshal(input, &in)
		if p := cmp.Or(in.FilePath, in.NotebookPath); p != "" && !within(dirs, p) {
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

func reservedCommand(script string) string {
	for _, argv := range simpleCommands(script) {
		if r := reservedArgv(argv); r != "" {
			return r
		}
	}
	return ""
}

// reservedArgv looks for git anywhere in a command, so that wrappers such as sudo, env or xargs need no rules of their own.
func reservedArgv(argv []string) string {
	for i, w := range argv {
		switch filepath.Base(w) {
		case "git":
			if r := reservedGit(argv[i+1:]); r != "" {
				return r
			}
		case "eval":
			return reservedCommand(strings.Join(argv[i+1:], " "))
		case "sh", "bash", "zsh", "dash":
			for j := i + 1; j+1 < len(argv); j++ {
				if a := argv[j]; len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'c') {
					return reservedCommand(argv[j+1])
				}
			}
		}
	}
	return ""
}

func reservedGit(args []string) string {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch args[i] {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace":
			i++
		}
	}
	if i >= len(args) {
		return ""
	}
	switch rest := args[i+1:]; args[i] {
	case "push":
		return "git push"
	case "branch":
		for _, a := range rest {
			if a == "--delete" || len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a, "dD") {
				return "deleting a branch"
			}
		}
	case "update-ref":
		if slices.Contains(rest, "-d") {
			return "deleting a branch"
		}
	}
	return ""
}

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
