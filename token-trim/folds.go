package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	goRunRE          = regexp.MustCompile(`^=== RUN   (\S+)$`)
	goPassRE         = regexp.MustCompile(`^--- PASS: (\S+) \([0-9.]+s\)$`)
	pytestPassRE     = regexp.MustCompile(`^\S+::\S+ PASSED(?: +\[ *[0-9]+%\])?$`)
	jestPassRE       = regexp.MustCompile(`^ +[✓✔] .+(?: \([0-9]+ ms\))?$`)
	tapPassRE        = regexp.MustCompile(`^ok [0-9]+(?: - [^\r\n]+)?$`)
	tapPlanRE        = regexp.MustCompile(`^1\.\.[0-9]+$`)
	cmakeProgressRE  = regexp.MustCompile(`^\[ *[0-9]+%\] (?:Building (?:C|CXX) object|Linking (?:C|CXX) (?:executable|static library|shared library)) .+$`)
	cargoProgressRE  = regexp.MustCompile(`^ +(?:Compiling|Checking) [A-Za-z0-9_.-]+ v[0-9][^\r\n]*$`)
	npmFetchRE       = regexp.MustCompile(`^npm http fetch GET (?:200|304) https://\S+ [0-9]+ms(?: \(cache (?:miss|hit|revalidated)\))?$`)
	pipDownloadRE    = regexp.MustCompile(`^ *(?:Downloading|Using cached) [A-Za-z0-9_.+-]+(?:\.whl|\.tar\.gz|\.zip)(?: \([0-9.]+ [kMGT]?B\))?$`)
	searchLineRE     = regexp.MustCompile(`^([^\r\n:]+):([0-9]+:.*)$`)
	diagnosticLineRE = regexp.MustCompile(`^([^\r\n:]+):([0-9]+:[0-9]+: (?:warning|note):.*)$`)
)

// These reducers deliberately recognise only complete line records, not arbitrary runner output.
func foldTests(s string) string {
	if unsafeFoldText(s) {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	kind := ""
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		previous := kind
		switch {
		case line == "PASS" || line == "FAIL" || strings.HasPrefix(line, "ok  \t") || strings.HasPrefix(line, "FAIL\t"):
			kind = "Go"
		case strings.HasPrefix(line, "=") && strings.Contains(line, " test session starts "):
			kind = "pytest"
		case strings.HasPrefix(line, "PASS ") || strings.HasPrefix(line, "FAIL "):
			kind = "Jest"
		case line == "TAP version 13" || tapPlanRE.MatchString(line):
			kind = "TAP"
		}
		if previous != "" && kind != previous {
			return s
		}
	}
	if kind == "" {
		return s
	}
	var out strings.Builder
	stopped := false
	for i := 0; i < len(lines); {
		line := strings.TrimSuffix(lines[i], "\n")
		if testFailure(line, kind) {
			stopped = true // A failure's diagnostic block can contain output resembling passing records.
		}
		if stopped {
			out.WriteString(lines[i])
			i++
			continue
		}
		end, count := i, 0
		for end < len(lines) {
			n := passingRecord(lines, end, kind)
			if n == 0 {
				break
			}
			end += n
			count++
		}
		if count >= 3 {
			fmt.Fprintf(&out, "[token-trim: %d passing %s tests folded]\n", count, kind)
			i = end
		} else {
			out.WriteString(lines[i])
			i++
		}
	}
	return shorterFold(s, out.String())
}

func passingRecord(lines []string, i int, kind string) int {
	line := strings.TrimSuffix(lines[i], "\n")
	switch kind {
	case "Go":
		if run := goRunRE.FindStringSubmatch(line); run != nil && i+1 < len(lines) {
			if pass := goPassRE.FindStringSubmatch(strings.TrimSuffix(lines[i+1], "\n")); pass != nil && run[1] == pass[1] {
				return 2
			}
		}
	case "pytest":
		if pytestPassRE.MatchString(line) {
			return 1
		}
	case "Jest":
		if jestPassRE.MatchString(line) {
			return 1
		}
	case "TAP":
		if tapPassRE.MatchString(line) && !strings.Contains(line, " #") {
			return 1
		}
	}
	return 0
}

func testFailure(line, kind string) bool {
	line = strings.TrimSpace(line)
	switch kind {
	case "Go":
		return strings.HasPrefix(line, "--- FAIL:") || strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:") || line == "FAIL" || strings.HasPrefix(line, "FAIL\t")
	case "pytest":
		return strings.Contains(line, " FAILED") || strings.Contains(line, " ERROR") || strings.Contains(line, " FAILURES ") || strings.Contains(line, " ERRORS ") || strings.HasPrefix(line, "_ _ _")
	case "Jest":
		return strings.HasPrefix(line, "FAIL ") || strings.HasPrefix(line, "✕ ") || strings.HasPrefix(line, "× ") || strings.HasPrefix(line, "● ")
	case "TAP":
		return strings.HasPrefix(line, "not ok ") || strings.HasPrefix(line, "Bail out!")
	}
	return false
}

func foldBuild(s string) string {
	if unsafeFoldText(s) {
		return s
	}
	return foldProgressRecords(s, "build progress", func(line string) bool {
		return cmakeProgressRE.MatchString(line) || cargoProgressRE.MatchString(line)
	})
}

func foldInstall(s string) string {
	if unsafeFoldText(s) {
		return s
	}
	return foldProgressRecords(s, "install progress", func(line string) bool {
		return npmFetchRE.MatchString(line) || pipDownloadRE.MatchString(line)
	})
}

func foldProgressRecords(s, label string, match func(string) bool) string {
	lines := strings.SplitAfter(s, "\n")
	var out strings.Builder
	stopped := false
	for i := 0; i < len(lines); {
		line := strings.TrimSuffix(lines[i], "\n")
		if progressFailure(line) {
			stopped = true
		}
		if stopped {
			out.WriteString(lines[i])
			i++
			continue
		}
		end := i
		for end < len(lines) && match(strings.TrimSuffix(lines[end], "\n")) {
			end++
		}
		if end-i >= 3 {
			fmt.Fprintf(&out, "[token-trim: %d %s lines folded]\n", end-i, label)
			i = end
		} else {
			out.WriteString(lines[i])
			i++
		}
	}
	return shorterFold(s, out.String())
}

func progressFailure(line string) bool {
	line = strings.ToLower(strings.TrimSpace(line))
	return strings.Contains(line, "error:") || strings.HasPrefix(line, "error ") ||
		strings.HasPrefix(line, "fatal") || strings.HasPrefix(line, "failed") ||
		strings.HasPrefix(line, "npm err!") || strings.HasPrefix(line, "npm error") || strings.HasPrefix(line, "ninja: build stopped")
}

// Group only adjacent equal paths; each suffix and its line ending is preserved verbatim.
func foldSearch(s string) string      { return groupPaths(s, searchLineRE) }
func foldDiagnostics(s string) string { return groupPaths(s, diagnosticLineRE) }

func groupPaths(s string, pattern *regexp.Regexp) string {
	if unsafeFoldText(s) {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	var out strings.Builder
	for i := 0; i < len(lines); {
		first := pattern.FindStringSubmatch(strings.TrimSuffix(lines[i], "\n"))
		if first == nil || strings.TrimSpace(first[1]) != first[1] {
			out.WriteString(lines[i])
			i++
			continue
		}
		end := i + 1
		for end < len(lines) {
			m := pattern.FindStringSubmatch(strings.TrimSuffix(lines[end], "\n"))
			if m == nil || m[1] != first[1] {
				break
			}
			end++
		}
		header := fmt.Sprintf("[token-trim path %s; %d lines; prepend path to each line]\n", strconv.Quote(first[1]), end-i)
		if end-i >= 3 && len(header) < (end-i)*len(first[1]) {
			out.WriteString(header)
			for _, line := range lines[i:end] {
				out.WriteString(line[len(first[1]):])
			}
			i = end
		} else {
			out.WriteString(lines[i])
			i++
		}
	}
	return shorterFold(s, out.String())
}

func shorterFold(original, folded string) string {
	if len(folded) < len(original) {
		return folded
	}
	return original
}

func structuredFoldText(s string) bool {
	for line := range strings.SplitSeq(s, "\n") {
		if goRunRE.MatchString(line) || goPassRE.MatchString(line) ||
			pytestPassRE.MatchString(line) || jestPassRE.MatchString(line) || tapPassRE.MatchString(line) || tapPlanRE.MatchString(line) ||
			strings.Contains(line, " test session starts ") ||
			strings.HasPrefix(line, "TAP version ") || strings.HasPrefix(line, "PASS ") || strings.HasPrefix(line, "FAIL ") ||
			cmakeProgressRE.MatchString(line) || cargoProgressRE.MatchString(line) ||
			npmFetchRE.MatchString(line) || pipDownloadRE.MatchString(line) || progressFailure(line) {
			return true
		}
		if m := searchLineRE.FindStringSubmatch(line); m != nil && !strings.ContainsAny(m[1], " \t") {
			return true
		}
	}
	return false
}

func allowedTextBits(t target, on mask) mask {
	allowed := textBits&^foldBits | foldMask(t)
	if on&foldBits != 0 && protectedFoldContext(t) {
		allowed &^= bitRepeats
	}
	return allowed
}

func protectedFoldContext(t target) bool {
	if t.failed || unsafeFoldText(t.text) {
		return true
	}
	if (patterns{"read", "read_file", "readfile", "edit", "write", "write_file", "apply_patch", "patch",
		"functions.read", "functions.edit", "functions.write", "functions.apply_patch"}).match(t.tool) {
		return true
	}
	words := strings.Fields(t.command)
	if len(words) == 0 {
		return false
	}
	program := strings.Trim(words[0], "\"'")
	if slash := strings.LastIndexByte(program, '/'); slash >= 0 {
		program = program[slash+1:]
	}
	switch program {
	case "cat", "bat", "head", "tail", "sed", "awk", "less", "more", "patch":
		return true
	case "git":
		return len(words) > 1 && (words[1] == "diff" || words[1] == "show" || words[1] == "apply")
	}
	return false
}

func unsafeFoldText(s string) bool {
	for line := range strings.SplitSeq(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") || strings.HasPrefix(strings.TrimSpace(line), "~~~") {
			return true
		}
		if numberedLine.MatchString(line) || strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "*** Begin Patch") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- ") && !strings.HasPrefix(line, "--- PASS:") && !strings.HasPrefix(line, "--- FAIL:") {
			return true
		}
	}
	return false
}

// Lossy folds require a recognised command; missing calls and ambiguous shell programs stay raw.
func foldMask(t target) mask {
	if t.failed || unsafeFoldText(t.text) {
		return 0
	}
	switch strings.ToLower(t.tool) {
	case "grep", "rg", "ripgrep", "search", "functions.grep", "functions.search":
		return bitSearch
	case "bash", "shell", "run", "exec", "exec_command", "run_shell_command", "execute", "terminal", "functions.bash", "functions.exec_command":
	default:
		return 0
	}
	command := strings.TrimSpace(t.command)
	if strings.ContainsAny(command, "\n\r;|&<>`$(){}") {
		return 0
	}
	words := strings.Fields(command)
	if len(words) == 0 {
		return 0
	}
	switch words[0] {
	case "pytest", "jest":
		return bitTests
	case "go":
		if len(words) > 1 && words[1] == "test" {
			return bitTests | bitDiagnostics
		}
		if len(words) > 1 && words[1] == "build" {
			return bitBuild | bitDiagnostics
		}
	case "cargo":
		if len(words) > 1 && (words[1] == "build" || words[1] == "check") {
			return bitBuild | bitDiagnostics
		}
	case "cmake":
		if len(words) > 1 && words[1] == "--build" {
			return bitBuild | bitDiagnostics
		}
	case "ninja":
		return bitBuild | bitDiagnostics
	case "npm":
		if len(words) > 1 && words[1] == "test" {
			return bitTests
		}
		if len(words) > 1 && (words[1] == "install" || words[1] == "ci") {
			return bitInstall
		}
	case "npx":
		if len(words) > 1 && words[1] == "jest" {
			return bitTests
		}
	case "node":
		if len(words) > 1 && words[1] == "--test" {
			return bitTests
		}
	case "python", "python3":
		if len(words) > 2 && words[1] == "-m" && words[2] == "pytest" {
			return bitTests
		}
		if len(words) > 3 && words[1] == "-m" && words[2] == "pip" && words[3] == "install" {
			return bitInstall
		}
	case "pip", "pip3":
		if len(words) > 1 && words[1] == "install" {
			return bitInstall
		}
	case "rg", "grep":
		return bitSearch
	}
	return 0
}

func callCommand(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return ""
		}
		raw = json.RawMessage(text)
	}
	var args struct {
		Command json.RawMessage `json:"command"`
		Cmd     string          `json:"cmd"`
	}
	if json.Unmarshal(raw, &args) != nil {
		return ""
	}
	if args.Cmd != "" {
		return args.Cmd
	}
	var command string
	if json.Unmarshal(args.Command, &command) == nil {
		return command
	}
	// Shell argv arrays are ambiguous (notably sh -c), so do not reconstruct them.
	return ""
}
