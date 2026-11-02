package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func goPassingOutput() string {
	var out strings.Builder
	for i := range 5 {
		fmt.Fprintf(&out, "=== RUN   TestLongPassingExample%d\n--- PASS: TestLongPassingExample%d (0.00s)\n", i, i)
	}
	return out.String()
}

func TestFoldPassingTestsKeepsFailureTail(t *testing.T) {
	goPass := goPassingOutput()
	cases := []struct {
		name, prefix, passes, failure, marker string
	}{
		{"Go", "", goPass, "=== RUN   TestBad\n--- FAIL: TestBad (0.01s)\n    x_test.go:12: expected true\n" + goPass + "FAIL\nFAIL\texample/project\t0.1s\n", "5 passing Go tests folded"},
		{"pytest", "============================= test session starts ==============================\n", "tests/test_long.py::test_alpha PASSED [ 10%]\ntests/test_long.py::test_beta PASSED [ 20%]\ntests/test_long.py::test_gamma PASSED [ 30%]\n", "tests/test_long.py::test_bad FAILED [ 40%]\nE   AssertionError: exact diagnostic\ntests/test_long.py::test_after PASSED [ 50%]\n", "3 passing pytest tests folded"},
		{"Jest", "PASS tests/long.test.js\n", "  ✓ very long passing case alpha (1 ms)\n  ✓ very long passing case beta (2 ms)\n  ✓ very long passing case gamma (3 ms)\n", "FAIL tests/bad.test.js\n  ● exact diagnostic\n    Expected: 1\n    Received: 2\n  ✓ after the failure (1 ms)\n", "3 passing Jest tests folded"},
		{"TAP", "TAP version 13\n", "ok 1 - very long passing case alpha\nok 2 - very long passing case beta\nok 3 - very long passing case gamma\n", "not ok 4 - bad\n  ---\n  message: exact diagnostic\n  ...\nok 5 - after the failure\n1..5\n", "3 passing TAP tests folded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.prefix + tc.passes + tc.failure
			got := foldTests(input)
			if !strings.Contains(got, tc.marker) || !strings.HasPrefix(got, tc.prefix) || !strings.HasSuffix(got, tc.failure) {
				t.Fatalf("fold lost context or failure:\n%s", got)
			}
			if again := foldTests(input); again != got {
				t.Fatal("fold is not deterministic")
			}
		})
	}
}

func TestFoldTestsKeepsUnknownAndLoggedRecords(t *testing.T) {
	input := "=== RUN   TestLogged\n    x_test.go:5: keep this log exactly\n--- PASS: TestLogged (0.00s)\n" + goPassingOutput() + "unrecognised runner trailer\nPASS\nok  \texample/project\t0.1s\n"
	got := foldTests(input)
	if !strings.HasPrefix(got, "=== RUN   TestLogged\n    x_test.go:5: keep this log exactly\n--- PASS: TestLogged (0.00s)\n") || !strings.HasSuffix(got, "unrecognised runner trailer\nPASS\nok  \texample/project\t0.1s\n") {
		t.Fatalf("unknown output or logged record changed: %q", got)
	}
	for _, input := range []string{
		goPassingOutput(),                         // No Go runner summary: could be file contents.
		"ok 1 - test\nok 2 - test\nok 3 - test\n", // No TAP header or plan.
		"...F...\nAssertionError: exact\n",        // pytest compact mode is not parsed.
		"PASS\n    --- PASS: TestIndented (0.00s)\n",
		"TAP version 13\nok 1 - skipped # SKIP reason\nok 2 - todo # TODO reason\n1..2\n",
	} {
		if got := foldTests(input); got != input {
			t.Fatalf("unsupported output changed: %q", got)
		}
	}
}

func TestFoldBuildAndInstallKeepsDiagnostics(t *testing.T) {
	cases := []struct {
		name, progress, failure, marker string
		fold                            func(string) string
	}{
		{"cmake", "[ 10%] Building CXX object path/alpha.cpp.o\n[ 20%] Building CXX object path/beta.cpp.o\n[ 30%] Building CXX object path/gamma.cpp.o\n", "source.cpp:14:3: error: exact message\n  code();\n  ^~~~~~\n", "3 build progress lines folded", foldBuild},
		{"cargo", "   Compiling long_dependency_alpha v1.0.0\n   Compiling long_dependency_beta v1.0.0\n    Checking long_dependency_gamma v1.0.0\n", "error: could not compile project\n  --> src/main.rs:8:2\n", "3 build progress lines folded", foldBuild},
		{"npm", "npm http fetch GET 200 https://registry.npmjs.org/alpha 12ms (cache miss)\nnpm http fetch GET 200 https://registry.npmjs.org/beta 13ms (cache hit)\nnpm http fetch GET 304 https://registry.npmjs.org/gamma 14ms (cache revalidated)\n", "npm ERR! code ERESOLVE\nexact dependency conflict\n", "3 install progress lines folded", foldInstall},
		{"pip", "Downloading long_dependency_alpha-1.0.0.whl (20 kB)\nUsing cached long_dependency_beta-1.0.0.whl (30 kB)\nDownloading long_dependency_gamma-1.0.0.tar.gz (40 kB)\n", "ERROR: Could not install packages\nexact diagnostic\n", "3 install progress lines folded", foldInstall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tail := "unknown line kept exactly\n" + tc.failure + tc.progress
			input := tc.progress + tail
			got := tc.fold(input)
			if !strings.Contains(got, tc.marker) || !strings.HasSuffix(got, tail) {
				t.Fatalf("progress fold lost diagnostic tail: %q", got)
			}
			if got := tc.fold(tc.failure + tc.progress); got != tc.failure+tc.progress {
				t.Fatal("fold changed failure diagnostic block")
			}
		})
	}
	unknown := "custom build step 1\ncustom build step 2\ncustom build step 3\n"
	if foldBuild(unknown) != unknown || foldInstall(unknown) != unknown {
		t.Fatal("unrecognised build/install output changed")
	}
}

func TestPathGroupingIsLossless(t *testing.T) {
	path := "src/a/very/long/project/directory/component/source.go"
	for _, diagnostic := range []bool{false, true} {
		var raw strings.Builder
		for i := range 8 {
			if diagnostic {
				fmt.Fprintf(&raw, "%s:%d:2: warning: keep exact message %d\n", path, i+1, i)
			} else {
				fmt.Fprintf(&raw, "%s:%d:  literal: match %d\n", path, i+1, i)
			}
		}
		input := raw.String() + "unrecognised line\n" + path + ":99:2: error: failure stays exact"
		got := foldSearch(input)
		if diagnostic {
			got = foldDiagnostics(input)
		}
		header := fmt.Sprintf("[token-trim path %s; 8 lines; prepend path to each line]\n", strconv.Quote(path))
		if !strings.HasPrefix(got, header) {
			t.Fatalf("missing lossless grouping header: %q", got)
		}
		grouped := strings.TrimPrefix(got, header)
		lines := strings.SplitAfter(grouped, "\n")
		for i := range 8 {
			lines[i] = path + lines[i]
		}
		if restored := strings.Join(lines, ""); restored != input {
			t.Fatalf("grouping lost content: %q", restored)
		}
	}
	errors := strings.Repeat(path+":1:2: error: unchanged\n", 8)
	if got := foldDiagnostics(errors); got != errors {
		t.Fatal("error diagnostics were rewritten")
	}
	short := "a.go:1:x\na.go:2:y\na.go:3:z\n"
	if foldSearch(short) != short {
		t.Fatal("grouping expanded short output")
	}
}

func TestFoldsProtectSourceAndPatches(t *testing.T) {
	passes := goPassingOutput() + "PASS\n"
	for _, input := range []string{
		"     1→\tcontents\n" + passes,
		"diff --git a/test.log b/test.log\n" + passes,
		"*** Begin Patch\n" + passes,
		"@@ -1,2 +1,3 @@\n" + passes,
	} {
		for _, fold := range []func(string) string{foldTests, foldBuild, foldInstall, foldSearch, foldDiagnostics} {
			if got := fold(input); got != input {
				t.Fatalf("file or patch changed: %q", got)
			}
		}
	}
}

func foldRequest(format, tool, command, output string, failed bool) []byte {
	args := map[string]any{"command": command}
	var body any
	switch format {
	case "claude":
		body = map[string]any{"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call", "name": tool, "input": args}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call", "content": output, "is_error": failed}}},
		}}
	case "chat":
		encoded, _ := json.Marshal(args)
		status := "completed"
		if failed {
			status = "failed"
		}
		body = map[string]any{"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call", "function": map[string]any{"name": tool, "arguments": string(encoded)}}}},
			map[string]any{"role": "tool", "tool_call_id": "call", "content": output, "status": status},
		}}
	case "responses":
		encoded, _ := json.Marshal(args)
		status := "completed"
		if failed {
			status = "error"
		}
		body = map[string]any{"input": []any{
			map[string]any{"type": "function_call", "call_id": "call", "name": tool, "arguments": string(encoded)},
			map[string]any{"type": "function_call_output", "call_id": "call", "output": output, "status": status},
		}}
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

func TestFoldToolAndErrorGating(t *testing.T) {
	output := goPassingOutput() + "PASS\n"
	for _, format := range []string{"claude", "chat", "responses"} {
		for _, tc := range []struct {
			tool, command string
			failed, folds bool
		}{
			{"Bash", "go test -v ./...", false, true},
			{"exec_command", "go test -v ./...", false, true},
			{"Read", "go test -v ./...", false, false},
			{"apply_patch", "go test -v ./...", false, false},
			{"unknown", "go test -v ./...", false, false},
			{"Bash", "cat output.log", false, false},
			{"Bash", "go test -v ./...; cat source.go", false, false},
			{"Bash", "go test -v ./...", true, false},
			{"Bash", "", false, false},
		} {
			body := foldRequest(format, tc.tool, tc.command, output, tc.failed)
			res, err := trim(body, rules{On: bitTests, Apply: true}, noCache{}, noPins{})
			if err != nil {
				t.Fatal(err)
			}
			if (len(res.Edits) > 0) != tc.folds {
				t.Fatalf("%s/%s/%s failed=%v: edits=%v", format, tc.tool, tc.command, tc.failed, res.Edits)
			}
			_, _, targets, err := locate(body)
			if err != nil || len(targets) != 1 || targets[0].failed != tc.failed {
				t.Fatalf("error metadata missing: %v %v", targets, err)
			}
		}
	}
}

type fixedFoldPin form

func (p fixedFoldPin) pinned([32]byte) (form, bool) { return form(p), true }

func TestOldPinnedMasksKeepTheirRendering(t *testing.T) {
	output := "\x1b[31m" + goPassingOutput() + "PASS\n\x1b[0m"
	body := foldRequest("claude", "Bash", "go test -v ./...", output, false)
	// Bit 0 was ANSI before folds existed; a pinned old output must not acquire the test reducer.
	res, err := trim(body, rules{On: textBits, Apply: true}, noCache{}, fixedFoldPin(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Edits) != 1 || res.Edits[0].Replacement != encodeString(stripANSI(output)) {
		t.Fatalf("old pin rendering changed: %v", res.Edits)
	}
	if bitANSI != 1 || bitProgress != 2 || bitRepeats != 4 || bitJSON != 8 || bitDedupe != 16 || bitOverstrike != 32 || bitControl != 64 || bitJSONL != 128 || formDedupe != 1<<15 {
		t.Fatal("existing persisted bits changed")
	}
}

func TestModernMasksProtectRepeatsWithoutChangingOldPins(t *testing.T) {
	var errors strings.Builder
	for i := range 20 {
		fmt.Fprintf(&errors, "source.go:%d:2: error: keep diagnostic %d exactly\n", i+1, i)
	}
	input := errors.String()
	if got, _ := clean(input, bitRepeats|bitTests); got != input {
		t.Fatalf("modern mask collapsed diagnostics: %q", got)
	}
	if got, _ := clean(input, bitRepeats); got != collapseRepeats(input) || got == input {
		t.Fatal("legacy repeat mask changed")
	}
	log := strings.Repeat("a line of ordinary log output 100\n", 20)
	if got, _ := clean(log, bitRepeats|foldBits); got == log {
		t.Fatal("modern mask disabled repeats for ordinary logs")
	}
	for _, tc := range []struct{ tool, command string }{
		{"Read", ""},
		{"apply_patch", ""},
		{"Bash", "cat source.go"},
		{"Bash", "git diff"},
	} {
		body := foldRequest("claude", tc.tool, tc.command, log, false)
		res, err := trim(body, rules{On: bitRepeats | foldBits, Apply: true}, noCache{}, noPins{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Edits) != 0 {
			t.Fatalf("fresh read/patch context collapsed: %s %s", tc.tool, tc.command)
		}
		res, err = trim(body, rules{On: bitRepeats | foldBits, Apply: true}, noCache{}, fixedFoldPin(bitRepeats))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Edits) != 1 || res.Edits[0].Replacement != encodeString(collapseRepeats(log)) {
			t.Fatal("old pinned repeats no longer render identically")
		}
	}
	body := foldRequest("claude", "Bash", "go test", log, true)
	res, err := trim(body, rules{On: bitRepeats | foldBits, Apply: true}, noCache{}, noPins{})
	if err != nil || len(res.Edits) != 0 {
		t.Fatalf("failed tool's fresh output collapsed: %v %v", res.Edits, err)
	}
}
