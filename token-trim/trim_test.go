package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var all = rules{On: textBits | bitDedupe, DedupeMin: 200, Apply: true}

type noCache struct{}

func (noCache) clean(_ [32]byte, m mask, text string) (string, map[string]int) { return clean(text, m) }

type noPins struct{}

func (noPins) pinned([32]byte) (form, bool) { return formRaw, false }

// splice applies edits like the host does (pluginhost.applyRequestBodyEdits); edits arrive in order.
func splice(body []byte, edits []edit) []byte {
	var out []byte
	pos := 0
	for _, e := range edits {
		out = append(append(out, body[pos:e.Start]...), e.Replacement...)
		pos = e.End
	}
	return append(out, body[pos:]...)
}

func longLog(tag string) string {
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "\x1b[2m[%s] step %d of 40 done\x1b[0m\n", tag, i)
	}
	return b.String()
}

// toolBody is a Claude Messages body where each call is a tool_use answered by its tool_result.
func toolBody(model string, calls ...[2]string) []byte {
	var uses, results []any
	for i, c := range calls {
		id := fmt.Sprintf("toolu_%d", i)
		uses = append(uses, map[string]any{"type": "tool_use", "id": id, "name": c[0], "input": map[string]any{}})
		results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id, "content": c[1]})
	}
	b, _ := json.Marshal(map[string]any{"model": model, "messages": []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": uses},
		map[string]any{"role": "user", "content": results},
	}})
	return b
}

func TestTextRules(t *testing.T) {
	if got := stripANSI("\x1b[1;32mPASS\x1b[0m ok \x1b]8;;http://x\x07link\x1b]8;;\x07"); got != "PASS ok link" {
		t.Errorf("ansi = %q", got)
	}
	if got := collapseCR("start\nDownloading 10%\rDownloading 55%\rDownloading 100%\r\nwin line\r\nend"); got != "start\nDownloading 100%\r\nwin line\r\nend" {
		t.Errorf("progress = %q", got)
	}
	// Overstrike renders like a terminal: a backspace steps back and the next character overwrites.
	for in, want := range map[string]string{
		"N\bNA\bAM\bME\bE and _\bu_\bn_\bd_\be_\br": "NAME and under",
		"|\b/\b-":    "-",
		"abc\b\b":    "abc", // nothing typed over: still shown
		"a\b\b\bxyz": "xyz",
	} {
		if got := applyBackspaces(in); got != want {
			t.Errorf("overstrike(%q) = %q, want %q", in, got, want)
		}
	}
	if got := dropControl("a\x00b\x07c\x0cd\te\x1bf\bg\r\n\x7f"); got != "abcd\te\x1bf\bg\r\n" {
		t.Errorf("control = %q", got)
	}
	if got := compactJSONLines("{\"a\": 1, \"b\": [1, 2]}\n\n{\"a\": 2}\n"); got != "{\"a\":1,\"b\":[1,2]}\n\n{\"a\":2}\n" {
		t.Errorf("jsonl = %q", got)
	}
	for _, s := range []string{"{\"a\": 1}\nplain text\n", "{\"a\": 1}", "{\n  \"a\": 1\n}", "{\"a\":1}\n{\"b\":2}"} {
		if compactJSONLines(s) != s {
			t.Errorf("jsonl changed %q", s)
		}
	}

	var logs []string
	for i := range 30 {
		logs = append(logs, fmt.Sprintf("2026-10-30 12:00:%02d INFO request %d served in %dms", i, 1000+i, 3+i))
	}
	got := collapseRepeats("build start\n" + strings.Join(logs, "\n") + "\nbuild done")
	want := "build start\n" + logs[0] + "\n" + logs[1] + "\n[… 26 similar lines omitted by token-trim]\n" + logs[28] + "\n" + logs[29] + "\nbuild done"
	if got != want {
		t.Errorf("repeats =\n%s", got)
	}
	// Never collapsed: numeric data rows (no letters), short runs, and line-numbered file reads whose
	// lines differ in content.
	var csv, file []string
	for i := range 30 {
		csv = append(csv, fmt.Sprintf("%d,%d,%d", i, i*2, i*3))
		file = append(file, fmt.Sprintf("%6d→\tfield%c := %d", i+1, 'a'+i%26, i))
	}
	for name, s := range map[string]string{"csv": strings.Join(csv, "\n"), "short": strings.Join(logs[:11], "\n"), "file": strings.Join(file, "\n")} {
		if collapseRepeats(s) != s {
			t.Errorf("%s was collapsed", name)
		}
	}
	// A code table read through Claude Code's Read tool differs only in numbers line to line: the
	// repeats rule must not touch a line-numbered file view (edits quote those lines exactly).
	var table []string
	for i := range 30 {
		table = append(table, fmt.Sprintf("%6d→\t{\"case%d\", %d},", i+1, i, i))
	}
	if out, _ := clean(strings.Join(table, "\n"), textBits); out != strings.Join(table, "\n") {
		t.Errorf("file view was collapsed:\n%s", out)
	}

	pretty := "{\n  \"name\": \"a b\",\n  \"list\": [\n    1,\n    2\n  ],\n  \"html\": \"<b>&</b>\"\n}\n"
	if got := compactJSON(pretty); got != `{"name":"a b","list":[1,2],"html":"<b>&</b>"}` {
		t.Errorf("json = %q", got)
	}
	for _, s := range []string{"not json {", "{ broken: 1 }", `"just a string"`} {
		if compactJSON(s) != s {
			t.Errorf("compactJSON changed %q", s)
		}
	}
	out, saved := clean("\x1b[31m{\n  \"a\": 1\n}\x1b[0m", textBits)
	if out != `{"a":1}` || saved[ruleANSI] != 9 || saved[ruleJSON] != 5 {
		t.Errorf("clean = %q %v", out, saved)
	}
}

func TestTrimFormats(t *testing.T) {
	logA, logB := longLog("a"), longLog("b")
	// The user text and system prompt carry escape codes too: only tool outputs may change.
	claude, _ := json.Marshal(map[string]any{
		"model":  "claude-sonnet-5-5",
		"system": "keep \x1b[1mthis\x1b[0m",
		"messages": []any{
			map[string]any{"role": "user", "content": "run it \x1b[31mnow\x1b[0m"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "make"}}}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": logA, "cache_control": map[string]any{"type": "ephemeral"}},
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": []any{map[string]any{"type": "text", "text": logB}, map[string]any{"type": "image", "source": map[string]any{"data": "AAAA"}}}},
			}},
		},
	})
	chat, _ := json.Marshal(map[string]any{"model": "gpt-6", "messages": []any{
		map[string]any{"role": "user", "content": "go \x1b[0m"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_9", "type": "function", "function": map[string]any{"name": "shell", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "call_9", "content": logA},
	}})
	responses, _ := json.Marshal(map[string]any{"model": "gpt-6-sol", "instructions": "x \x1b[0m", "input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi \x1b[0m"}}},
		map[string]any{"type": "function_call", "call_id": "call_3", "name": "exec", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_3", "output": logA},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_4", "output": []any{map[string]any{"type": "input_text", "text": logB}}},
	}})
	cases := []struct {
		name, format string
		body         []byte
		tools        []string
		keep         []string
	}{
		{"claude", "claude", claude, []string{"Bash", ""}, []string{`keep \u001b[1mthis`, `run it \u001b[31mnow`, `"cache_control":{"type":"ephemeral"}`, `"data":"AAAA"`}},
		{"chat", "openai", chat, []string{"shell"}, []string{`go \u001b[0m`}},
		{"responses", "openai-response", responses, []string{"exec", ""}, []string{`x \u001b[0m`, `hi \u001b[0m`}},
	}
	for _, c := range cases {
		res, err := trim(c.body, all, noCache{}, noPins{})
		if err != nil || res.Format != c.format || res.Outputs != len(c.tools) || res.Trimmed != len(c.tools) || res.Saved <= 0 || res.Saved != res.Would {
			t.Fatalf("%s: %+v %v", c.name, res, err)
		}
		var tools []string
		for _, f := range res.fresh {
			tools = append(tools, f.tool)
		}
		if fmt.Sprint(tools) != fmt.Sprint(c.tools) {
			t.Errorf("%s: tools = %q, want %q", c.name, tools, c.tools)
		}
		out := splice(c.body, res.Edits)
		if !json.Valid(out) {
			t.Fatalf("%s: patched body is not JSON:\n%s", c.name, out)
		}
		for _, k := range c.keep {
			if !strings.Contains(string(out), k) {
				t.Errorf("%s: non-tool content changed, lost %q", c.name, k)
			}
		}
		if strings.Contains(string(out), `step 20 of 40`) || !strings.Contains(string(out), "similar lines omitted by token-trim") {
			t.Errorf("%s: tool output not trimmed:\n%s", c.name, out)
		}
		// Same body, same edits: the rewritten history is identical every turn (prompt cache stays warm).
		again, _ := trim(c.body, all, noCache{}, noPins{})
		if fmt.Sprint(again.Edits) != fmt.Sprint(res.Edits) {
			t.Errorf("%s: trimming is not deterministic", c.name)
		}
		// Measuring never edits but reports the same savings.
		measure := all
		measure.Apply = false
		if m, _ := trim(c.body, measure, noCache{}, noPins{}); len(m.Edits) != 0 || m.Saved != 0 || m.Would != res.Saved || m.Trimmed != res.Trimmed {
			t.Errorf("%s: measure = %+v", c.name, m)
		}
	}
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"hello \u001b[0m"}]}`)
	if res, err := trim(plain, all, noCache{}, noPins{}); err != nil || res.Outputs != 0 || len(res.Edits) != 0 {
		t.Errorf("body without tools = %+v %v", res, err)
	}
}

func TestDedupeAndThresholds(t *testing.T) {
	var lines []string
	for i := range 80 {
		lines = append(lines, fmt.Sprintf("func handle%c%c() { return } // same file read twice", 'a'+i%26, 'a'+i/26))
	}
	big := strings.Join(lines, "\n")
	small := strings.Repeat("x", 100)
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_A", "content": big},
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_B", "content": big},
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_C", "content": small},
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_D", "content": small},
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_E", "content": "\x1b[1mtiny\x1b[0m"},
		}},
	}})
	r := all
	r.MinChars = 50
	res, err := trim(body, r, noCache{}, noPins{})
	if err != nil || res.Trimmed != 1 || res.Rules[ruleDedupe] == 0 {
		t.Fatalf("dedupe = %+v %v", res, err)
	}
	out := string(splice(body, res.Edits))
	if strings.Count(out, "same file read twice") != 80 || !strings.Contains(out, "identical to the output of tool call toolu_A above") {
		t.Errorf("first copy must stay, second must point to it:\n%.300s", out)
	}
	if !strings.Contains(out, "\\u001b[1mtiny") {
		t.Error("output under min_output_chars was trimmed")
	}
}

func corpus(t testing.TB) map[string][]byte {
	files, _ := filepath.Glob("testdata/*.json")
	if len(files) == 0 {
		t.Fatal("no request bodies in testdata")
	}
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = b
	}
	return out
}

// gaps is body with every tool output string cut out: what must never change.
func gaps(body []byte, ts []target) string {
	var b strings.Builder
	prev := 0
	for _, t := range ts {
		b.Write(body[prev:t.start])
		b.WriteString("\x00")
		prev = t.end
	}
	b.Write(body[prev:])
	return b.String()
}

// TestCorpus replays request bodies shaped like what Claude Code, Codex, OpenAI Chat clients and OpenCode
// send: the patched body must be valid JSON, identical outside tool outputs, and each output must be its
// cleaned form or a reference to an identical earlier one.
func TestCorpus(t *testing.T) {
	for name, body := range corpus(t) {
		if !json.Valid(body) {
			t.Fatalf("%s: fixture is not JSON", name)
		}
		res, err := trim(body, all, noCache{}, noPins{})
		if err != nil || res.Unsafe != "" || res.Trimmed == 0 {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
		patched := splice(body, res.Edits)
		if !json.Valid(patched) {
			t.Fatalf("%s: patched body is not JSON", name)
		}
		_, _, before, _ := locate(body)
		_, _, after, err := locate(patched)
		if err != nil || len(after) != len(before) || len(res.fresh) != len(before) {
			t.Fatalf("%s: %d outputs before, %d after (%v)", name, len(before), len(after), err)
		}
		if gaps(body, before) != gaps(patched, after) {
			t.Errorf("%s: bytes outside tool outputs changed", name)
		}
		for i := range before {
			got, orig := after[i].text, before[i].text
			cleaned, _ := clean(orig, textBits)
			if got != orig && got != cleaned && !strings.HasPrefix(got, "[token-trim: identical to the output of tool call") {
				t.Errorf("%s: output %d became %q", name, i, got)
			}
			if res.fresh[i].tool == "" {
				t.Errorf("%s: output %d has no tool name", name, i)
			}
			if res.fresh[i].tool == "Read" && got != orig {
				t.Errorf("%s: a line-numbered file read changed", name)
			}
		}
		if again, _ := trim(body, all, noCache{}, noPins{}); fmt.Sprint(again.Edits) != fmt.Sprint(res.Edits) {
			t.Errorf("%s: not deterministic", name)
		}
	}
}

// FuzzTrim: whatever the body, trimming must not panic and a patched body must stay valid JSON.
func FuzzTrim(f *testing.F) {
	for _, body := range corpus(f) {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		res, err := trim(body, all, noCache{}, noPins{})
		if err == nil && len(res.Edits) > 0 && !json.Valid(splice(body, res.Edits)) {
			t.Fatalf("patched body is not JSON: %q", splice(body, res.Edits))
		}
	})
}

func TestVerifyEdits(t *testing.T) {
	body := []byte(`{"a":"xx","b":"yy"}`)
	if err := verifyEdits(body, []edit{{5, 9, `"x"`}, {14, 18, `"y"`}}); err != nil {
		t.Errorf("valid edits rejected: %v", err)
	}
	for name, edits := range map[string][]edit{
		"overlap":      {{5, 9, `"x"`}, {7, 12, `"z"`}},
		"out of range": {{5, 99, `"x"`}},
		"broken JSON":  {{5, 9, `"x`}},
	} {
		if verifyEdits(body, edits) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestShadowAndSkipLists(t *testing.T) {
	logA := longLog("a")
	r := all
	r.On &^= bitANSI | bitDedupe
	res, _ := trim(toolBody("claude-x", [2]string{"Bash", logA}, [2]string{"Bash", logA}), r, noCache{}, noPins{})
	if res.Rules[ruleANSI] != 0 || res.Shadow[ruleANSI] == 0 || res.Shadow[ruleDedupe] == 0 || res.Shadow[ruleRepeats] != 0 || res.Rules[ruleRepeats] == 0 {
		t.Errorf("disabled rules must be measured apart: rules %v shadow %v", res.Rules, res.Shadow)
	}

	r = all
	r.SkipTools = patterns{"bash", "mcp__*"}
	body := toolBody("claude-x", [2]string{"Bash", logA}, [2]string{"mcp__git__log", longLog("m")}, [2]string{"Grep", longLog("g")})
	res, _ = trim(body, r, noCache{}, noPins{})
	if res.Skipped != 2 || len(res.Edits) != 1 || !strings.Contains(string(splice(body, res.Edits)), `[a] step 20 of 40`) {
		t.Errorf("skip_tools: %+v", res)
	}
	r = all
	r.SkipModels = patterns{"claude-*"}
	if res, _ = trim(body, r, noCache{}, noPins{}); res.Skipped != 3 || len(res.Edits) != 0 || res.Would != 0 {
		t.Errorf("skip_models: %+v", res)
	}
	if (patterns{"read"}).match("Reader") || !(patterns{"read"}).match("Read") || (patterns{"x*"}).match("") {
		t.Error("pattern matching")
	}
}

// TestCachePinning: an output keeps the form it was first sent in, whatever the mode or rules later.
func TestCachePinning(t *testing.T) {
	cfg := defaultSettings()
	cfg.DataDir = t.TempDir()
	s := newService()
	s.configure(cfg)
	logA, logB, logC := longLog("a"), longLog("b"), longLog("c")
	turn1 := toolBody("claude-x", [2]string{"Bash", logA})
	turn2 := toolBody("claude-x", [2]string{"Bash", logA}, [2]string{"Bash", logB})

	if out := s.patch(turn1); len(out) != 0 { // measuring: sent raw, pinned raw
		t.Fatalf("measure returned edits: %v", out)
	}
	cfg.Mode = modeApply
	s.configure(cfg)
	edits, _ := s.patch(turn2)["edits"].([]edit)
	patched := string(splice(turn2, edits))
	if len(edits) != 1 || !strings.Contains(patched, `[a] step 20 of 40`) || strings.Contains(patched, `[b] step 20 of 40`) {
		t.Fatalf("switching to apply must leave the already-sent output as it was: %v", edits)
	}
	if day := s.stats(1).Today; day.KeptOutputs != 1 || day.Kept == 0 || day.NewSaved == 0 {
		t.Errorf("kept accounting: %+v", day)
	}

	cfg.ANSI = false // a rule change must not alter outputs already sent ...
	s.configure(cfg)
	if again, _ := s.patch(turn2)["edits"].([]edit); fmt.Sprint(again) != fmt.Sprint(edits) {
		t.Errorf("rule change rewrote a sent output:\n%v\n%v", again, edits)
	}
	turn3 := toolBody("claude-x", [2]string{"Bash", logA}, [2]string{"Bash", logB}, [2]string{"Bash", logC})
	e3, _ := s.patch(turn3)["edits"].([]edit)
	if len(e3) != 2 || !strings.Contains(e3[1].Replacement, `\u001b[2m[c]`) || strings.Contains(e3[0].Replacement, `\u001b`) {
		t.Errorf("... while a new output gets the new rules: %v", e3)
	}

	s.stop() // pins survive a restart
	s2 := newService()
	s2.configure(cfg)
	defer s2.stop()
	if again, _ := s2.patch(turn2)["edits"].([]edit); fmt.Sprint(again) != fmt.Sprint(edits) {
		t.Errorf("pins lost on restart:\n%v\n%v", again, edits)
	}
}

func TestPinsFile(t *testing.T) {
	cfg := defaultSettings()
	cfg.DataDir = t.TempDir()
	s := newService()
	s.configure(cfg)
	s.mu.Lock()
	s.pinLocked([32]byte{1}, form(bitANSI), true)
	s.pinLocked([32]byte{2}, formDedupe, true)
	s.pinLocked([32]byte{1}, formRaw, true) // already pinned: the first form stays
	s.mu.Unlock()
	s.stop()
	path := filepath.Join(cfg.DataDir, pinsFile)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{9, 9, 9}) // a record cut short by a crash
	f.Close()

	s2 := newService()
	s2.configure(cfg)
	defer s2.stop()
	f1, ok1 := s2.pinned([32]byte{1})
	f2, ok2 := s2.pinned([32]byte{2})
	if !ok1 || f1 != form(bitANSI) || !ok2 || f2 != formDedupe {
		t.Errorf("reloaded pins = %v %v / %v %v", f1, ok1, f2, ok2)
	}
	if st, _ := os.Stat(path); st.Size() != 2*pinRecord {
		t.Errorf("pins.bin not compacted: %d bytes", st.Size())
	}
}

func TestPatchModesAndAccounting(t *testing.T) {
	s := newService()
	body := toolBody("claude-x", [2]string{"Bash", longLog("x")})
	if out := s.patch(body); len(out) != 0 { // default mode: measure
		t.Fatalf("measure mode returned edits: %v", out)
	}
	s.cfg.Mode = modeApply
	if out := s.patch(body); len(out) != 0 { // the output went out raw while measuring: it stays raw
		t.Fatalf("apply rewrote a pinned output: %v", out)
	}
	s.cfg.Mode = modeOff
	s.patch(body)
	day := s.stats(1).Today
	if day.Requests != 2 || day.Would == 0 || day.NewWould != day.Would || day.Saved != 0 || day.KeptOutputs != 1 || day.Kept != day.Would {
		t.Errorf("accounting: %+v", day)
	}
	if day.Rules[ruleANSI].Hits != 1 || day.Models["claude-x"].Hits != 2 || len(s.stats(1).Recent) != 1 {
		t.Errorf("rules/models/recent = %+v %+v", day.Rules, day.Models)
	}
	if day.Tools["Bash"].Outputs != 1 || day.Tools["Bash"].Saved != day.Would || len(s.stats(1).Biggest) != 1 {
		t.Errorf("tools count new outputs once: %+v", day.Tools["Bash"])
	}
	s.cfg.Mode = modeMeasure
	if out := s.patch([]byte(`{"messages":[{"role":"tool","content":`)); len(out) != 0 || s.stats(1).Today.Errors != 1 {
		t.Error("a broken body must pass unchanged and count as an error")
	}
}

func TestBiggestOutputs(t *testing.T) {
	s := newService()
	now := time.Now().UnixMilli()
	s.data.Biggest = []bigOutput{{At: now - 40*24*3600*1000, Chars: 1 << 30}} // older than the window
	for i := range maxBiggest + 5 {
		s.noteBiggestLocked(bigOutput{At: now, Chars: 1000 + i})
	}
	v := s.stats(1).Biggest
	if len(v) != maxBiggest || v[0].Chars != 1000+maxBiggest+4 || v[len(v)-1].Chars != 1005 {
		t.Errorf("biggest = %d entries, first %d, last %d", len(v), v[0].Chars, v[len(v)-1].Chars)
	}
}

func TestRegisterDeclaresPatcher(t *testing.T) {
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte("data_dir: " + t.TempDir() + "\nmode: apply\nrule_json: false\nskip_tools: [\" Read \", \"\"]\nprices: {claude-x: {input: 3}}\n")})
	raw, err := handleMethod("plugin.register", req)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.stop()
	if !strings.Contains(string(raw), `"request_body_patcher_before":true`) || !strings.Contains(string(raw), `"usage_plugin":true`) || svc.cfg.Mode != modeApply || svc.cfg.JSON || !svc.cfg.ANSI {
		t.Fatalf("register = %s cfg %+v", raw, svc.cfg)
	}
	if fmt.Sprint(svc.cfg.SkipTools) != "[Read]" || svc.cfg.Prices["claude-x"].Input != 3 {
		t.Errorf("lists = %q prices = %v", svc.cfg.SkipTools, svc.cfg.Prices)
	}
}

func usageRecord(model, session, auth string, at time.Time, latency, ttft time.Duration, d usageDetail) []byte {
	raw, _ := json.Marshal(map[string]any{"Model": model, "SessionID": session, "AuthID": auth, "RequestedAt": at, "Latency": latency, "TTFT": ttft, "Detail": d})
	return raw
}

func TestConsumption(t *testing.T) {
	// Detail fields as each provider's parser in the host fills them.
	for name, c := range map[string]struct {
		d    usageDetail
		want used
	}{
		// Claude: input excludes cache, output includes thinking, total = sum of all four.
		"claude": {usageDetail{InputTokens: 10, OutputTokens: 50, ReasoningTokens: 20, CacheReadTokens: 900, CacheCreationTokens: 40, TotalTokens: 1000},
			used{Requests: 1, Input: 10, CacheRead: 900, CacheWrite: 40, Output: 50, Total: 1000}},
		// OpenAI: prompt tokens include the cached ones, completion includes reasoning.
		"openai": {usageDetail{InputTokens: 1000, OutputTokens: 80, ReasoningTokens: 30, CacheReadTokens: 700, TotalTokens: 1080},
			used{Requests: 1, Input: 300, CacheRead: 700, Output: 80, Total: 1080}},
		// Gemini: thoughts are reported apart from the candidates and counted in the total.
		"gemini": {usageDetail{InputTokens: 500, OutputTokens: 40, ReasoningTokens: 60, CacheReadTokens: 200, TotalTokens: 600},
			used{Requests: 1, Input: 300, CacheRead: 200, Output: 100, Total: 600}},
		"no total": {usageDetail{InputTokens: 7, OutputTokens: 3}, used{Requests: 1, Input: 7, Output: 3, Total: 10}},
	} {
		if got := consumption(c.d, false); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}

	s := newService()
	s.data.Days[time.Now().AddDate(0, 0, -400).Format(time.DateOnly)] = &bucket{Used: used{Requests: 1, Total: 999}} // another month
	today := &bucket{}
	json.Unmarshal([]byte(`{"rules":{},"models":{},"formats":{}}`), today) // a day saved by an older version
	s.data.Days[time.Now().Format(time.DateOnly)] = today
	now := time.Now()
	s.usage([]byte(`{"Model":"m","Detail":{"InputTokens":10,"OutputTokens":5,"TotalTokens":15}}`))
	s.usage(usageRecord("fast", "", "auth-a", now, 3*time.Second, time.Second, usageDetail{InputTokens: 50, OutputTokens: 200, TotalTokens: 250}))
	s.usage([]byte(`{"Failed":true,"Detail":{}}`))
	s.usage([]byte(`not json`))
	v := s.stats(1)
	want := used{Requests: 3, Failed: 1, Input: 60, Output: 205, Total: 265}
	if v.Today.Used != want || v.Month != want || v.MonthUsage["fast"].Total != 250 {
		t.Errorf("today %+v month %+v, want %+v", v.Today.Used, v.Month, want)
	}
	// 200 tokens in 2 s after 1 s to first token: 100 tok/s falls in the 100–120 bucket, 1000 ms in 1000–1500.
	fast := v.Today.Speed["fast"]
	if len(v.Today.Speed) != 1 || fast == nil || fast.Requests != 1 || fast.Output != 200 || fast.GenMs != 2000 || fast.TTFTMs != 1000 ||
		fast.RateHist[9] != 1 || fast.TTFTHist[4] != 1 || fast.ByPrompt[0] != (promptTTFT{1, 1000}) {
		t.Errorf("speed by model = %+v (only the streamed reply is timed)", fast)
	}
	if v.Today.Accounts["auth-a"] == nil || v.Today.Accounts["auth-a"].Output != 200 {
		t.Errorf("speed by account = %+v", v.Today.Accounts)
	}
	if last := v.Live[len(v.Live)-1]; len(v.Live) != liveMinutes || last.Tokens != 265 || last.Requests != 3 || last.Speed.Output != 200 || v.Live[0].Tokens != 0 {
		t.Errorf("live = %+v", v.Live)
	}
	for name, c := range map[string]struct {
		out           int64
		latency, ttft time.Duration
		failed        bool
	}{
		"not streamed": {200, 3 * time.Second, 0, false},
		"failed":       {200, 3 * time.Second, time.Second, true},
		"tiny reply":   {5, 3 * time.Second, time.Second, false},
		"one chunk":    {200, time.Second + 50*time.Millisecond, time.Second, false},
	} {
		if _, ok := timing(c.out, 0, c.latency, c.ttft, c.failed); ok {
			t.Errorf("%s must not be timed", name)
		}
	}
	s.reset()
	if v := s.stats(1); v.Today.Used != want || v.Today.Speed["fast"] == nil || v.Today.Usage["fast"] == nil || v.Today.Requests != 0 {
		t.Errorf("reset must keep consumption and speed only: %+v", v.Today)
	}
}

func TestCacheHealth(t *testing.T) {
	s := newService()
	t0 := time.Now().Add(-time.Hour)
	rec := func(at time.Time, session string, cacheRead, cacheWrite int64) {
		s.usage(usageRecord("claude-x", session, "", at, 0, 0, usageDetail{InputTokens: 10, OutputTokens: 100, CacheReadTokens: cacheRead,
			CacheCreationTokens: cacheWrite, TotalTokens: 110 + cacheRead + cacheWrite}))
	}
	rec(t0, "s1", 0, 10_000)                      // first turn: everything written
	rec(t0.Add(time.Minute), "s1", 10_000, 500)   // history read from the cache
	rec(t0.Add(2*time.Minute), "s1", 0, 10_600)   // rebuilt one minute later: a break
	rec(t0.Add(20*time.Minute), "s1", 0, 10_700)  // rebuilt after a long pause: expired
	rec(t0.Add(21*time.Minute), "", 0, 50_000)    // no session id: not watched
	rec(t0.Add(22*time.Minute), "s2", 0, 5_000)   // a new conversation's first turn
	rec(t0.Add(23*time.Minute), "s2", 5_000, 100) // fine
	v := s.stats(1)
	u := v.Today.Usage["claude-x"]
	if u.Breaks != 1 || u.Expired != 1 {
		t.Errorf("breaks %d expired %d", u.Breaks, u.Expired)
	}
	if len(v.Breaks) != 1 || v.Breaks[0].GapS != 60 || v.Breaks[0].PrevPrompt != 10_510 || v.Breaks[0].Session == "s1" || v.Breaks[0].Mode != modeMeasure {
		t.Errorf("break list = %+v", v.Breaks)
	}
}
