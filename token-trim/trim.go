package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Rules are pure functions of one tool output: the same output always trims to the same text, so a
// conversation's history is rewritten identically on every turn and the provider's prompt cache keeps
// hitting. Nothing outside tool outputs is touched.

const (
	ruleANSI       = "ansi"
	ruleProgress   = "progress"
	ruleOverstrike = "overstrike"
	ruleControl    = "control"
	ruleRepeats    = "repeats"
	ruleJSON       = "json"
	ruleJSONL      = "jsonl"
	ruleDedupe     = "dedupe"
	repeatRun      = 12 // a run of similar lines is collapsed from this length on
	repeatKeep     = 2  // lines kept at each end of a collapsed run
)

// mask is a set of rules. The bit values are stored in cache pins (pins.bin): never renumber them.
type mask uint16

const (
	bitANSI       mask = 1 << 0
	bitProgress   mask = 1 << 1
	bitRepeats    mask = 1 << 2
	bitJSON       mask = 1 << 3
	bitDedupe     mask = 1 << 4
	bitOverstrike mask = 1 << 5
	bitControl    mask = 1 << 6
	bitJSONL      mask = 1 << 7
	textBits           = bitANSI | bitProgress | bitOverstrike | bitControl | bitRepeats | bitJSON | bitJSONL
)

// textRules run in this order on one output; dedupe works across outputs and is applied in trim.
var textRules = []struct {
	name string
	bit  mask
	fn   func(string) string
}{
	{ruleANSI, bitANSI, stripANSI},
	{ruleProgress, bitProgress, collapseCR},
	{ruleOverstrike, bitOverstrike, applyBackspaces},
	{ruleControl, bitControl, dropControl},
	{ruleRepeats, bitRepeats, collapseRepeats},
	{ruleJSON, bitJSON, compactJSON},
	{ruleJSONL, bitJSONL, compactJSONLines},
}

var ruleOrder = []string{ruleANSI, ruleProgress, ruleOverstrike, ruleControl, ruleRepeats, ruleJSON, ruleJSONL, ruleDedupe}

var ruleBits = map[string]mask{ruleANSI: bitANSI, ruleProgress: bitProgress, ruleOverstrike: bitOverstrike, ruleControl: bitControl,
	ruleRepeats: bitRepeats, ruleJSON: bitJSON, ruleJSONL: bitJSONL, ruleDedupe: bitDedupe}

// patterns are names matched case-insensitively, exactly or by a prefix ending in "*".
type patterns []string

func (p patterns) match(name string) bool {
	for _, pat := range p {
		if pre, ok := strings.CutSuffix(pat, "*"); ok && len(name) >= len(pre) && strings.EqualFold(name[:len(pre)], pre) {
			return true
		} else if !ok && strings.EqualFold(name, pat) {
			return true
		}
	}
	return false
}

type rules struct {
	On         mask
	MinChars   int  // outputs shorter than this are left alone
	DedupeMin  int  // duplicates shorter than this are left alone
	Apply      bool // requests go out trimmed; otherwise trimming is only measured
	SkipTools  patterns
	SkipModels patterns
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// stripANSI removes terminal colour and cursor escape sequences.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiRE.ReplaceAllString(s, "")
}

// collapseCR keeps what a terminal would finally show for lines redrawn with carriage returns
// (progress bars, spinners). A CRLF line ending is kept as is.
func collapseCR(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		crlf := strings.HasSuffix(line, "\r")
		body := strings.TrimSuffix(line, "\r")
		if !strings.Contains(body, "\r") {
			continue
		}
		segs := strings.Split(body, "\r")
		last := ""
		for _, seg := range segs {
			if seg != "" {
				last = seg
			}
		}
		if crlf {
			last += "\r"
		}
		lines[i] = last
	}
	return strings.Join(lines, "\n")
}

// similarity returns a line's shape with digit runs masked, or "" when the line is not eligible for
// collapsing: it needs a letter (so numeric data rows are never merged) and some substance.
func similarity(line string) string {
	t := strings.TrimSpace(line)
	if len(t) < 4 || strings.IndexFunc(t, unicode.IsLetter) < 0 {
		return ""
	}
	var b strings.Builder
	digit := false
	for _, r := range line {
		if r >= '0' && r <= '9' {
			if !digit {
				b.WriteByte('#')
			}
			digit = true
			continue
		}
		digit = false
		b.WriteRune(r)
	}
	return b.String()
}

// collapseRepeats shortens long runs of lines that differ only in their numbers (log timestamps,
// counters, "Downloading 41/300"): the first and last lines stay, the middle becomes one marker line.
func collapseRepeats(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) < repeatRun {
		return s
	}
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		sig := similarity(lines[i])
		j := i + 1
		for sig != "" && j < len(lines) && similarity(lines[j]) == sig {
			j++
		}
		if n := j - i; sig != "" && n >= repeatRun {
			out = append(out, lines[i:i+repeatKeep]...)
			out = append(out, fmt.Sprintf("[… %d similar lines omitted by token-trim]", n-2*repeatKeep))
			out = append(out, lines[j-repeatKeep:j]...)
		} else {
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	if len(out) == len(lines) {
		return s
	}
	return strings.Join(out, "\n")
}

// compactJSON minifies an output that is entirely one JSON value. Lossless: only whitespace between
// tokens goes; strings, numbers and key order are untouched.
func compactJSON(s string) string {
	t := strings.TrimSpace(s)
	if len(t) < 2 || (t[0] != '{' && t[0] != '[') {
		return s
	}
	var b bytes.Buffer
	if json.Compact(&b, []byte(t)) != nil || b.Len() >= len(s) {
		return s
	}
	return b.String()
}

// compactJSONLines minifies output where every non-empty line is one JSON object or array (JSON Lines,
// often written with ", " and ": " separators). Lossless like compactJSON.
func compactJSONLines(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) < 2 {
		return s
	}
	var b bytes.Buffer
	values, changed := 0, false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		b.Reset()
		if (t[0] != '{' && t[0] != '[') || json.Compact(&b, []byte(t)) != nil {
			return s
		}
		values++
		if b.Len() < len(line) {
			lines[i], changed = b.String(), true
		}
	}
	if values < 2 || !changed {
		return s
	}
	return strings.Join(lines, "\n")
}

// applyBackspaces renders terminal overstrike, as `man` and some build tools print it ("N\bN" for bold,
// "_\bx" for underline): a backspace moves back one character and the next one overwrites it.
func applyBackspaces(s string) string {
	if !strings.Contains(s, "\b") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "\b") {
			continue
		}
		var buf []rune
		cur := 0
		for _, r := range line {
			if r == '\b' {
				cur = max(cur-1, 0)
				continue
			}
			if cur < len(buf) {
				buf[cur] = r
			} else {
				buf = append(buf, r)
			}
			cur++
		}
		lines[i] = string(buf)
	}
	return strings.Join(lines, "\n")
}

// dropControl removes control characters a terminal does not print (NUL, BEL, vertical tab, form feed,
// DEL and the other C0 codes). Tabs, newlines, carriage returns, backspaces and escapes are left to the
// rules that handle them.
func dropControl(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\t' && r != '\n' && r != '\r' && r != '\b' && r != 0x1b) || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

var numberedLine = regexp.MustCompile(`^\s*\d+(→|\t)`)

// fileView reports a line-numbered file listing (Claude Code's Read tool, `cat -n`): its lines must
// stay exact because edits quote them, so the lossy repeats rule skips it.
func fileView(s string) bool {
	first, _, _ := strings.Cut(strings.TrimLeft(s, "\n"), "\n")
	return numberedLine.MatchString(first)
}

// clean runs the text rules in m in order; saved holds the bytes each rule removed.
func clean(s string, m mask) (string, map[string]int) {
	saved := map[string]int{}
	for _, r := range textRules {
		if m&r.bit == 0 || (r.bit == bitRepeats && fileView(s)) {
			continue
		}
		if t := r.fn(s); len(t) < len(s) {
			saved[r.name] += len(s) - len(t)
			s = t
		}
	}
	return s, saved
}

func dedupeMarker(id string) string {
	if id == "" {
		return "[token-trim: identical to an earlier tool output in this conversation]"
	}
	return fmt.Sprintf("[token-trim: identical to the output of tool call %s above]", id)
}

// ---- request bodies ----

// target is one tool output string inside a request body.
type target struct {
	start, end int    // byte span of the JSON string literal, quotes included
	text       string // decoded value
	id         string // tool call id, for duplicate references
	tool       string // name of the tool that produced it, when the call is in the body
}

type bodyDoc struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Input    json.RawMessage   `json:"input"`
}

type textPart struct {
	Type string `json:"type"`
}

// toolKinds are cheap substrings that every body with a tool output contains; bodies without any
// are skipped before parsing.
var toolKinds = [][]byte{[]byte(`"tool_result"`), []byte(`"tool"`), []byte(`"function_call_output"`), []byte(`"custom_tool_call_output"`)}

func hasToolOutput(body []byte) bool {
	for _, k := range toolKinds {
		if bytes.Contains(body, k) {
			return true
		}
	}
	return false
}

// textPaths records where a tool output lives: a plain string at path, or the text fields of the
// text parts of an array at path.
func textPaths(raw json.RawMessage, path, id string, partTypes map[string]bool, into map[string]string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	switch raw[0] {
	case '"':
		into[path] = id
	case '[':
		var parts []textPart
		if json.Unmarshal(raw, &parts) != nil {
			return
		}
		for k, p := range parts {
			if partTypes[p.Type] {
				into[path+"."+strconv.Itoa(k)+".text"] = id
			}
		}
	}
}

var (
	claudeParts   = map[string]bool{"text": true}
	responseParts = map[string]bool{"input_text": true, "output_text": true, "text": true}
)

// locate finds the tool outputs of a Claude Messages, OpenAI Chat or OpenAI Responses body.
func locate(body []byte) (model, format string, targets []target, err error) {
	var doc bodyDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", nil, err
	}
	paths := map[string]string{} // dotted path of a tool output string -> tool call id
	names := map[string]string{} // tool call id -> tool name
	for i, raw := range doc.Messages {
		var m struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		for _, tc := range m.ToolCalls {
			names[tc.ID] = tc.Function.Name
		}
		base := "messages." + strconv.Itoa(i) + ".content"
		if m.Role == "tool" {
			format = "openai"
			textPaths(m.Content, base, m.ToolCallID, claudeParts, paths)
			continue
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if c := bytes.TrimSpace(m.Content); len(c) == 0 || c[0] != '[' || json.Unmarshal(c, &blocks) != nil {
			continue
		}
		for j, b := range blocks {
			switch b.Type {
			case "tool_use":
				names[b.ID] = b.Name
			case "tool_result":
				format = "claude"
				textPaths(b.Content, base+"."+strconv.Itoa(j)+".content", b.ToolUseID, claudeParts, paths)
			}
		}
	}
	if in := bytes.TrimSpace(doc.Input); len(in) > 0 && in[0] == '[' {
		var items []struct {
			Type   string          `json:"type"`
			CallID string          `json:"call_id"`
			Name   string          `json:"name"`
			Output json.RawMessage `json:"output"`
		}
		if json.Unmarshal(in, &items) == nil {
			for i, it := range items {
				switch it.Type {
				case "function_call", "custom_tool_call":
					names[it.CallID] = it.Name
				case "function_call_output", "custom_tool_call_output":
					format = "openai-response"
					textPaths(it.Output, "input."+strconv.Itoa(i)+".output", it.CallID, responseParts, paths)
				}
			}
		}
	}
	if len(paths) == 0 {
		return doc.Model, format, nil, nil
	}
	err = walkStrings(body, func(path string, start, end int) {
		id, ok := paths[path]
		if !ok {
			return
		}
		var text string
		if json.Unmarshal(body[start:end], &text) == nil {
			targets = append(targets, target{start: start, end: end, text: text, id: id, tool: names[id]})
		}
	})
	return doc.Model, format, targets, err
}

// walkStrings calls fn with the dotted path ("messages.3.content.0.text") and byte span of every
// string value under the top-level "messages" and "input" keys, in document order.
func walkStrings(b []byte, fn func(path string, start, end int)) error {
	w := walker{b: b, fn: fn}
	w.ws()
	if w.i >= len(b) || b[w.i] != '{' {
		return errors.New("body is not a JSON object")
	}
	return w.object("", true, true)
}

type walker struct {
	b  []byte
	i  int
	fn func(string, int, int)
}

var errSyntax = errors.New("malformed JSON")

func (w *walker) ws() {
	for w.i < len(w.b) {
		switch w.b[w.i] {
		case ' ', '\t', '\n', '\r':
			w.i++
		default:
			return
		}
	}
}

// str scans the string starting at w.i and returns its span.
func (w *walker) str() (int, int, error) {
	start := w.i
	for w.i++; w.i < len(w.b); w.i++ {
		switch w.b[w.i] {
		case '\\':
			w.i++
		case '"':
			w.i++
			return start, w.i, nil
		}
	}
	return 0, 0, errSyntax
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func (w *walker) value(path string, visit bool) error {
	w.ws()
	if w.i >= len(w.b) {
		return errSyntax
	}
	switch w.b[w.i] {
	case '{':
		return w.object(path, false, visit)
	case '[':
		return w.array(path, visit)
	case '"':
		s, e, err := w.str()
		if err == nil && visit {
			w.fn(path, s, e)
		}
		return err
	}
	for w.i < len(w.b) && !strings.ContainsRune(",}] \t\n\r", rune(w.b[w.i])) {
		w.i++
	}
	return nil
}

// object walks an object; at the top level only "messages" and "input" are visited (the rest, e.g.
// tool definitions and the system prompt, is skipped without building paths).
func (w *walker) object(path string, top, visit bool) error {
	w.i++ // '{'
	w.ws()
	if w.i < len(w.b) && w.b[w.i] == '}' {
		w.i++
		return nil
	}
	for {
		w.ws()
		if w.i >= len(w.b) || w.b[w.i] != '"' {
			return errSyntax
		}
		ks, ke, err := w.str()
		if err != nil {
			return err
		}
		key := string(w.b[ks+1 : ke-1])
		if bytes.IndexByte(w.b[ks:ke], '\\') >= 0 {
			_ = json.Unmarshal(w.b[ks:ke], &key)
		}
		w.ws()
		if w.i >= len(w.b) || w.b[w.i] != ':' {
			return errSyntax
		}
		w.i++
		child, childPath := visit && (!top || key == "messages" || key == "input"), ""
		if child {
			childPath = join(path, key)
		}
		if err := w.value(childPath, child); err != nil {
			return err
		}
		w.ws()
		if w.i >= len(w.b) {
			return errSyntax
		}
		switch w.b[w.i] {
		case ',':
			w.i++
		case '}':
			w.i++
			return nil
		default:
			return errSyntax
		}
	}
}

func (w *walker) array(path string, visit bool) error {
	w.i++ // '['
	w.ws()
	if w.i < len(w.b) && w.b[w.i] == ']' {
		w.i++
		return nil
	}
	for n := 0; ; n++ {
		childPath := ""
		if visit {
			childPath = join(path, strconv.Itoa(n))
		}
		if err := w.value(childPath, visit); err != nil {
			return err
		}
		w.ws()
		if w.i >= len(w.b) {
			return errSyntax
		}
		switch w.b[w.i] {
		case ',':
			w.i++
		case ']':
			w.i++
			return nil
		default:
			return errSyntax
		}
	}
}

// encodeString renders s as a JSON string literal without HTML escaping.
func encodeString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// edit is a byte-range replacement in the request body (pluginapi.RequestBodyEdit).
type edit struct {
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement"`
}

// form is how a tool output goes upstream: raw, cleaned with a rule mask, or replaced by a reference to
// an identical earlier output. Stored in cache pins (pins.bin): keep the encoding.
type form uint16

const (
	formRaw    form = 0
	formDedupe form = 1 << 15
)

// pinStore knows the form each tool output was first sent in. Resending that same form keeps the
// conversation's prefix byte-identical, so changing mode or rules never breaks a cached conversation.
type pinStore interface {
	pinned(key [32]byte) (form, bool)
}

// pinKey identifies one tool output: its content and the tool call it answers.
func pinKey(content [32]byte, id string) [32]byte {
	h := sha256.New()
	h.Write(content[:])
	h.Write([]byte(id))
	var k [32]byte
	h.Sum(k[:0])
	return k
}

// freshOutput is a tool output seen for the first time; it is pinned once the request is sent.
type freshOutput struct {
	key   [32]byte
	form  form // the form it goes out in now
	tool  string
	chars int
	saved int // bytes the current rules remove from it
}

// result is what trimming one request body did (or would do).
type result struct {
	Model, Format     string
	Outputs, Trimmed  int            // tool outputs found / shortened (in the request sent when applying, else as measured)
	CharsIn           int            // decoded bytes of tool outputs
	Saved             int            // bytes removed from the request sent (apply)
	Would             int            // bytes the current rules remove, as if every output were new
	Kept, KeptOutputs int            // apply: bytes left in because outputs keep the form they were first sent in
	Skipped           int            // outputs left alone by skip_tools / skip_models
	Rules             map[string]int // bytes per rule, of Saved (apply) or Would (measure)
	Shadow            map[string]int // bytes the disabled rules would remove
	Unsafe            string         // the patched body failed verification and goes out unchanged
	Edits             []edit
	fresh             []freshOutput
}

// cleaner caches clean() by content hash and mask: a conversation resends the same outputs every turn.
type cleaner interface {
	clean(key [32]byte, m mask, text string) (string, map[string]int)
}

// trim finds the tool outputs in body and decides each one's form: a new output gets what the current
// settings want (sent only when applying), an output already sent keeps its pinned form.
func trim(body []byte, r rules, c cleaner, pins pinStore) (result, error) {
	res := result{Rules: map[string]int{}, Shadow: map[string]int{}}
	if !hasToolOutput(body) {
		return res, nil
	}
	model, format, targets, err := locate(body)
	res.Model, res.Format = model, format
	if err != nil {
		return res, err
	}
	skipModel := r.SkipModels.match(model)
	first := map[[32]byte]string{} // content hash -> tool call id of its first occurrence
	for _, t := range targets {
		res.Outputs++
		res.CharsIn += len(t.text)
		key := sha256.Sum256([]byte(t.text))
		prevID, dup := first[key]
		if !dup {
			first[key] = t.id
		}
		render := func(f form) (string, map[string]int) {
			switch f {
			case formRaw:
				return t.text, nil
			case formDedupe:
				if !dup { // the first copy is no longer in this request
					return t.text, nil
				}
				out := dedupeMarker(prevID)
				return out, map[string]int{ruleDedupe: len(t.text) - len(out)}
			}
			return c.clean(key, mask(f), t.text)
		}

		skip := skipModel || r.SkipTools.match(t.tool)
		want := formRaw
		switch {
		case skip:
			res.Skipped++
		case dup && r.On&bitDedupe != 0 && len(t.text) >= r.DedupeMin:
			want = formDedupe
		case len(t.text) >= r.MinChars:
			want = form(r.On & textBits)
		}
		wantOut, wantSaved := render(want)
		wantN := max(len(t.text)-len(wantOut), 0)
		res.Would += wantN
		if !skip && want != formDedupe && len(t.text) >= r.MinChars && r.On&textBits != textBits {
			_, all := c.clean(key, textBits, t.text)
			for name, n := range all {
				if r.On&ruleBits[name] == 0 {
					res.Shadow[name] += n
				}
			}
		}
		if !skip && dup && r.On&bitDedupe == 0 && len(t.text) >= r.DedupeMin {
			res.Shadow[ruleDedupe] += len(t.text) - len(dedupeMarker(prevID))
		}

		pk := pinKey(key, t.id)
		pinned, known := pins.pinned(pk)
		sent := formRaw
		if r.Apply {
			sent = want
			if known {
				sent = pinned
			}
		}
		if !known {
			res.fresh = append(res.fresh, freshOutput{key: pk, form: sent, tool: t.tool, chars: len(t.text), saved: wantN})
		}
		out, saved := wantOut, wantSaved
		if sent != want {
			out, saved = render(sent)
		}
		n := max(len(t.text)-len(out), 0)
		var lit string
		if r.Apply && n > 0 {
			if lit = encodeString(out); len(lit) >= t.end-t.start { // escaping made it longer: send as is
				n, saved = 0, nil
			}
		}
		if r.Apply && wantN > n {
			res.Kept += wantN - n
			res.KeptOutputs++
		}
		if !r.Apply {
			n, saved = wantN, wantSaved
		}
		if n == 0 {
			continue
		}
		res.Trimmed++
		for name, k := range saved {
			res.Rules[name] += k
		}
		if r.Apply {
			res.Saved += n
			res.Edits = append(res.Edits, edit{Start: t.start, End: t.end, Replacement: lit})
		}
	}
	if len(res.Edits) > 0 {
		if err := verifyEdits(body, res.Edits); err != nil {
			res.Unsafe = err.Error()
			res.Edits, res.Saved, res.Trimmed = nil, 0, 0
			clear(res.Rules)
			for i := range res.fresh {
				res.fresh[i].form = formRaw
			}
		}
	}
	return res, nil
}

// verifyEdits applies edits to a copy of body and checks the result is still valid JSON: the last check
// before a patched body leaves the proxy.
func verifyEdits(body []byte, edits []edit) error {
	var b bytes.Buffer
	b.Grow(len(body))
	prev := 0
	for _, e := range edits {
		if e.Start < prev || e.End < e.Start || e.End > len(body) {
			return errors.New("edits overlap or fall outside the body")
		}
		b.Write(body[prev:e.Start])
		b.WriteString(e.Replacement)
		prev = e.End
	}
	b.Write(body[prev:])
	if !json.Valid(b.Bytes()) {
		return errors.New("patched body is not valid JSON")
	}
	return nil
}
