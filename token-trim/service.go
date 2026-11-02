package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	version     = "0.5.0"
	modeOff     = "off"
	modeMeasure = "measure" // compute and record savings, send requests unchanged
	modeApply   = "apply"
	maxRecent   = 60
	maxCache    = 4096 // cleaned outputs kept by content hash and rule mask
	flushEvery  = time.Minute
	statsFile   = "stats.json"
	liveMinutes = 60 // per-minute consumption kept in memory for the live chart

	pinsFile   = "pins.bin" // append-only: 32-byte output key + 2-byte form per record
	pinRecord  = 34
	maxPins    = 200_000 // tool outputs whose first-sent form is remembered (~7 MB on disk)
	maxBiggest = 20      // largest new tool outputs kept, over biggestWindow

	cacheTTL       = 5 * time.Minute // Anthropic's default prompt cache lifetime
	minCachePrompt = 2048            // smaller prompts are not worth watching for cache breaks
	maxSessions    = 20_000
	maxBreaks      = 30
)

const biggestWindow = 30 * 24 * time.Hour

// price is $ per million tokens.
type price struct {
	Input      float64 `yaml:"input" json:"input"`
	CacheRead  float64 `yaml:"cache_read" json:"cache_read"`
	CacheWrite float64 `yaml:"cache_write" json:"cache_write"`
	Output     float64 `yaml:"output" json:"output"`
}

type settings struct {
	Mode            string           `yaml:"mode"`
	ANSI            bool             `yaml:"rule_ansi"`
	Progress        bool             `yaml:"rule_progress"`
	Overstrike      bool             `yaml:"rule_overstrike"`
	Control         bool             `yaml:"rule_control"`
	Repeats         bool             `yaml:"rule_repeats"`
	JSON            bool             `yaml:"rule_json"`
	JSONL           bool             `yaml:"rule_jsonl"`
	Dedupe          bool             `yaml:"rule_dedupe"`
	Tests           bool             `yaml:"rule_tests"`
	Build           bool             `yaml:"rule_build"`
	Install         bool             `yaml:"rule_install"`
	Search          bool             `yaml:"rule_search"`
	Diagnostics     bool             `yaml:"rule_diagnostics"`
	ArchiveEnabled  bool             `yaml:"archive_enabled"`
	ArchiveListen   string           `yaml:"archive_listen"`
	ArchiveMaxBytes int64            `yaml:"archive_max_bytes"`
	HistoryBudget   int              `yaml:"history_budget_bytes"`
	HistoryRecent   int              `yaml:"history_recent_bytes"`
	OutputLimit     int              `yaml:"output_limit_bytes"`
	MinChars        int              `yaml:"min_output_chars"`
	DedupeMin       int              `yaml:"dedupe_min_chars"`
	SkipTools       []string         `yaml:"skip_tools"`
	SkipModels      []string         `yaml:"skip_models"`
	Prices          map[string]price `yaml:"prices"`
	DataDir         string           `yaml:"data_dir"`
	HistoryDays     int              `yaml:"history_days"`
}

func defaultSettings() settings {
	return settings{Mode: modeMeasure, ANSI: true, Progress: true, Overstrike: true, Control: true, Repeats: true, JSON: true, JSONL: true, Dedupe: true,
		Tests: true, Build: true, Install: true, Search: true, Diagnostics: true,
		ArchiveListen: "127.0.0.1:8319", ArchiveMaxBytes: 1 << 30, HistoryBudget: 600000, HistoryRecent: 150000, OutputLimit: 24576,
		MinChars: 200, DedupeMin: 2000, DataDir: "token-trim-data", HistoryDays: 90}
}

func parseSettings(raw []byte) (settings, error) {
	s := defaultSettings()
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &s); err != nil {
			return s, err
		}
	}
	switch s.Mode {
	case modeOff, modeMeasure, modeApply:
	default:
		s.Mode = modeMeasure
	}
	s.MinChars = max(s.MinChars, 0)
	s.DedupeMin = max(s.DedupeMin, 200)
	s.HistoryDays = min(max(s.HistoryDays, 1), 400)
	if strings.TrimSpace(s.DataDir) == "" {
		s.DataDir = defaultSettings().DataDir
	}
	s.SkipTools, s.SkipModels = names(s.SkipTools), names(s.SkipModels)
	if s.ArchiveListen == "" {
		s.ArchiveListen = defaultSettings().ArchiveListen
	}
	if s.ArchiveMaxBytes < 65536 || s.HistoryBudget < 8192 || s.HistoryRecent < 2048 || s.HistoryRecent > s.HistoryBudget/2 || s.OutputLimit < 1024 {
		return s, fmt.Errorf("archive limits require max bytes >= 65536, history budget >= 8192, recent >= 2048 and <= half budget, output limit >= 1024")
	}
	return s, nil
}

// names trims list entries and drops the empty ones.
func names(list []string) []string {
	out := []string{}
	for _, n := range list {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func (s settings) switches() map[string]bool {
	return map[string]bool{ruleANSI: s.ANSI, ruleProgress: s.Progress, ruleOverstrike: s.Overstrike, ruleControl: s.Control,
		ruleRepeats: s.Repeats, ruleJSON: s.JSON, ruleJSONL: s.JSONL, ruleDedupe: s.Dedupe,
		ruleTests: s.Tests, ruleBuild: s.Build, ruleInstall: s.Install, ruleSearch: s.Search, ruleDiagnostics: s.Diagnostics}
}

func (s settings) rules() rules {
	var on mask
	for name, enabled := range s.switches() {
		if enabled {
			on |= ruleBits[name]
		}
	}
	return rules{On: on, MinChars: s.MinChars, DedupeMin: s.DedupeMin, Apply: s.Mode == modeApply, SkipTools: s.SkipTools, SkipModels: s.SkipModels}
}

type configField struct {
	Name        string
	Type        string
	Default     any
	EnumValues  []string `json:",omitempty"`
	Description string
}

func configFields() []configField {
	d := defaultSettings()
	return []configField{
		{Name: "mode", Type: "enum", Default: d.Mode, EnumValues: []string{modeOff, modeMeasure, modeApply}, Description: "off: do nothing. measure: record what would be saved, send requests unchanged. apply: send trimmed tool outputs."},
		{Name: "rule_ansi", Type: "boolean", Default: d.ANSI, Description: "Remove terminal colour/cursor escape codes from tool outputs."},
		{Name: "rule_progress", Type: "boolean", Default: d.Progress, Description: "Keep only the final state of lines redrawn with carriage returns (progress bars)."},
		{Name: "rule_overstrike", Type: "boolean", Default: d.Overstrike, Description: "Render backspace overstrike (bold/underline in man pages) as a terminal shows it."},
		{Name: "rule_control", Type: "boolean", Default: d.Control, Description: "Drop control characters a terminal does not print (NUL, BEL, form feed…)."},
		{Name: "rule_repeats", Type: "boolean", Default: d.Repeats, Description: "Collapse runs of 12+ lines that differ only in numbers (logs, counters) to first 2 + last 2."},
		{Name: "rule_json", Type: "boolean", Default: d.JSON, Description: "Minify tool outputs that are one pretty-printed JSON value (lossless)."},
		{Name: "rule_jsonl", Type: "boolean", Default: d.JSONL, Description: "Minify JSON Lines outputs one line at a time (lossless)."},
		{Name: "rule_dedupe", Type: "boolean", Default: d.Dedupe, Description: "Replace a repeat of an earlier identical tool output with a reference to it."},
		{Name: "rule_tests", Type: "boolean", Default: d.Tests, Description: "Fold recognised passing test lines; retain failures, diagnostics and totals."},
		{Name: "rule_build", Type: "boolean", Default: d.Build, Description: "Fold recognised successful compiler progress, preserving diagnostics."},
		{Name: "rule_install", Type: "boolean", Default: d.Install, Description: "Fold recognised package download/install progress."},
		{Name: "rule_search", Type: "boolean", Default: d.Search, Description: "Group recognised search matches by path without dropping matches or line numbers."},
		{Name: "rule_diagnostics", Type: "boolean", Default: d.Diagnostics, Description: "Group recognised diagnostics without dropping messages."},
		{Name: "archive_enabled", Type: "boolean", Default: false, Description: "Opt into private archives and MCP retrieval. Requires authenticated CallerScope and advertised retrieval tools before shortening archived content."},
		{Name: "archive_listen", Type: "string", Default: d.ArchiveListen, Description: "MCP listener address. Publish only over a trusted local network or TLS; bearer tokens grant access to one caller/workspace."},
		{Name: "archive_max_bytes", Type: "integer", Default: d.ArchiveMaxBytes, Description: "Archive disk quota including durable views. Full storage leaves content unchanged; referenced entries are never auto-evicted."},
		{Name: "history_budget_bytes", Type: "integer", Default: d.HistoryBudget, Description: "Archive complete old conversation groups when request bytes exceed this budget. Not an exact token limit."},
		{Name: "history_recent_bytes", Type: "integer", Default: d.HistoryRecent, Description: "Recent conversation bytes targeted after a history cut; always retain the newest eight entries."},
		{Name: "output_limit_bytes", Type: "integer", Default: d.OutputLimit, Description: "Large eligible tool outputs get a recoverable preview; file views, patches, errors and retrieval results are excluded."},
		{Name: "min_output_chars", Type: "integer", Default: d.MinChars, Description: "Tool outputs shorter than this are left alone."},
		{Name: "dedupe_min_chars", Type: "integer", Default: d.DedupeMin, Description: "Only duplicates at least this long are replaced (minimum 200)."},
		{Name: "skip_tools", Type: "array", Default: []string{}, Description: `Tool names whose outputs are never trimmed, e.g. ["Read", "mcp__*"] (case-insensitive, * at the end = prefix).`},
		{Name: "skip_models", Type: "array", Default: []string{}, Description: `Models whose requests are never trimmed, e.g. ["claude-opus-*"].`},
		{Name: "prices", Type: "object", Default: map[string]price{}, Description: `$ per million tokens by model (exact name or prefix*), e.g. {"claude-sonnet-*": {"input": 3, "cache_read": 0.3, "cache_write": 3.75, "output": 15}}.`},
		{Name: "data_dir", Type: "string", Default: d.DataDir, Description: "Directory for stats.json and pins.bin, relative to the server's working directory."},
		{Name: "history_days", Type: "integer", Default: d.HistoryDays, Description: "Days of daily statistics kept."},
	}
}

// ---- statistics ----

type counter struct {
	Hits  int64 `json:"hits"`
	Saved int64 `json:"saved"`
	New   int64 `json:"new"` // of Saved, from outputs trimmed for the first time
}

// used is token consumption reported by the providers for every request through the proxy.
type used struct {
	Requests   int64 `json:"requests"`
	Failed     int64 `json:"failed"`
	Input      int64 `json:"input"` // input not served from cache
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"` // including reasoning
	Total      int64 `json:"total"`
}

func (u *used) add(o used) {
	u.Requests += o.Requests
	u.Failed += o.Failed
	u.Input += o.Input
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Output += o.Output
	u.Total += o.Total
}

type usageDetail struct {
	InputTokens, OutputTokens, ReasoningTokens, CacheReadTokens, CacheCreationTokens, TotalTokens int64
}

// consumption splits one usage record into non-overlapping parts. Providers disagree on what overlaps:
// Claude's input excludes cache reads and its output includes thinking; OpenAI's input includes cached
// tokens; Gemini reports thoughts apart from output. The host's total is consistent, so uncached input is
// what remains of it after output and cache.
func consumption(d usageDetail, failed bool) used {
	u := used{Requests: 1, CacheRead: d.CacheReadTokens, CacheWrite: d.CacheCreationTokens, Output: d.OutputTokens, Total: d.TotalTokens}
	if u.Total == 0 {
		u.Total = d.InputTokens + d.OutputTokens
	}
	if r := d.ReasoningTokens; r > 0 && u.Total == d.InputTokens+d.OutputTokens+r { // reasoning reported apart (Gemini)
		u.Output += r
	}
	u.Input = max(0, u.Total-u.Output-u.CacheRead-u.CacheWrite)
	if failed {
		u.Failed = 1
	}
	return u
}

// Speed histograms: a reply falls in the first bucket whose edge is above its value, the last bucket is
// open-ended. Percentiles are read from them on the page.
var (
	rateEdges   = [...]float64{10, 20, 30, 40, 50, 60, 70, 80, 100, 120, 150, 200, 250, 300, 400, 500, 750, 1000}   // tokens/s
	ttftEdges   = [...]float64{250, 500, 750, 1000, 1500, 2000, 3000, 4000, 6000, 8000, 12000, 20000, 30000, 60000} // ms
	promptEdges = [...]float64{8_000, 32_000, 128_000}                                                              // prompt tokens
)

func bucketOf(edges []float64, v float64) int {
	i := 0
	for i < len(edges) && v >= edges[i] {
		i++
	}
	return i
}

type promptTTFT struct {
	Requests int64 `json:"requests"`
	TTFTMs   int64 `json:"ttft_ms"`
}

// speed sums streamed replies' timing: output tokens over the time spent generating them.
type speed struct {
	Requests int64                            `json:"requests"`
	Output   int64                            `json:"output"`
	GenMs    int64                            `json:"gen_ms"` // latency minus time to first token
	TTFTMs   int64                            `json:"ttft_ms"`
	RateHist [len(rateEdges) + 1]int64        `json:"rate_hist"`
	TTFTHist [len(ttftEdges) + 1]int64        `json:"ttft_hist"`
	ByPrompt [len(promptEdges) + 1]promptTTFT `json:"by_prompt"` // time to first token by prompt size
}

func (p *speed) add(o speed) {
	p.Requests += o.Requests
	p.Output += o.Output
	p.GenMs += o.GenMs
	p.TTFTMs += o.TTFTMs
	for i := range p.RateHist {
		p.RateHist[i] += o.RateHist[i]
	}
	for i := range p.TTFTHist {
		p.TTFTHist[i] += o.TTFTHist[i]
	}
	for i := range p.ByPrompt {
		p.ByPrompt[i].Requests += o.ByPrompt[i].Requests
		p.ByPrompt[i].TTFTMs += o.ByPrompt[i].TTFTMs
	}
}

// timing is one request's speed sample. Only streamed replies have a time to first token, which separates
// reading the prompt from generating; tiny or instant replies (a buffered stream arrives in one piece) are
// skipped because their rate is noise.
func timing(output, prompt int64, latency, ttft time.Duration, failed bool) (speed, bool) {
	gen := latency - ttft
	if failed || ttft <= 0 || output < 10 || gen < 100*time.Millisecond {
		return speed{}, false
	}
	p := speed{Requests: 1, Output: output, GenMs: gen.Milliseconds(), TTFTMs: ttft.Milliseconds()}
	p.RateHist[bucketOf(rateEdges[:], float64(output)/gen.Seconds())]++
	p.TTFTHist[bucketOf(ttftEdges[:], float64(p.TTFTMs))]++
	c := &p.ByPrompt[bucketOf(promptEdges[:], float64(prompt))]
	c.Requests, c.TTFTMs = 1, p.TTFTMs
	return p, true
}

// modelUse is one model's consumption and how its prompt cache behaved.
type modelUse struct {
	used
	Breaks  int64 `json:"breaks"`  // cached history rebuilt mid-conversation (within the cache lifetime)
	Expired int64 `json:"expired"` // rebuilt after a pause longer than the cache lifetime (normal)
}

func (m *modelUse) add(o modelUse) {
	m.used.add(o.used)
	m.Breaks += o.Breaks
	m.Expired += o.Expired
}

// toolStat counts each tool's new outputs (not their resends): which tools produce the noise.
type toolStat struct {
	Outputs int64 `json:"outputs"`
	Chars   int64 `json:"chars"`
	Saved   int64 `json:"saved"` // bytes the current rules remove
}

type bigOutput struct {
	At    int64  `json:"at"`
	Tool  string `json:"tool"`
	Model string `json:"model"`
	Chars int    `json:"chars"`
	Saved int    `json:"saved"`
}

type cacheBreak struct {
	At         int64  `json:"at"`
	Model      string `json:"model"`
	Session    string `json:"session"` // short hash, never the id itself
	Prompt     int64  `json:"prompt"`
	PrevPrompt int64  `json:"prev_prompt"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
	GapS       int64  `json:"gap_s"`
	Mode       string `json:"mode"` // token-trim's mode at the time
}

type minute struct {
	At       int64 `json:"at"` // minute start, ms
	Tokens   int64 `json:"tokens"`
	Requests int64 `json:"requests"`
	Speed    speed `json:"speed"` // streamed replies finished this minute
}

// bucket is one day of activity. Byte counts are of decoded tool output text (≈ 4 bytes per token).
type bucket struct {
	Used           used                 `json:"used"`
	Requests       int64                `json:"requests"`        // request bodies scanned
	WithTools      int64                `json:"with_tools"`      // of which carried tool outputs
	Trimmed        int64                `json:"trimmed"`         // of which had something trimmed
	Outputs        int64                `json:"outputs"`         // tool outputs scanned
	TrimmedOutputs int64                `json:"trimmed_outputs"` // tool outputs shortened
	CharsIn        int64                `json:"chars_in"`        // tool output bytes scanned
	Saved          int64                `json:"saved"`           // bytes removed from requests sent (apply)
	NewSaved       int64                `json:"new_saved"`       // of which from outputs trimmed for the first time
	Would          int64                `json:"would"`           // bytes that would have been removed (measure)
	NewWould       int64                `json:"new_would"`
	Kept           int64                `json:"kept"`         // apply: bytes left in so cached conversations stay identical
	KeptOutputs    int64                `json:"kept_outputs"` // outputs resent in their first-sent form instead of trimmed
	Skipped        int64                `json:"skipped"`      // outputs left alone by skip_tools / skip_models
	Errors         int64                `json:"errors"`       // bodies that could not be parsed (sent unchanged)
	Unsafe         int64                `json:"unsafe"`       // patched bodies that failed verification (sent unchanged)
	Rules          map[string]*counter  `json:"rules"`
	Shadow         map[string]int64     `json:"shadow"` // bytes disabled rules would have removed
	Models         map[string]*counter  `json:"models"`
	Tools          map[string]*toolStat `json:"tools"`
	Formats        map[string]int64     `json:"formats"`
	Speed          map[string]*speed    `json:"speed"`    // by model
	Accounts       map[string]*speed    `json:"accounts"` // by credential
	Usage          map[string]*modelUse `json:"usage"`    // consumption by model
}

func newBucket() *bucket {
	b := &bucket{}
	b.ensure()
	return b
}

// ensure creates the maps a day saved by an older version lacks.
func (b *bucket) ensure() {
	if b.Rules == nil {
		b.Rules = map[string]*counter{}
	}
	if b.Shadow == nil {
		b.Shadow = map[string]int64{}
	}
	if b.Models == nil {
		b.Models = map[string]*counter{}
	}
	if b.Tools == nil {
		b.Tools = map[string]*toolStat{}
	}
	if b.Formats == nil {
		b.Formats = map[string]int64{}
	}
	if b.Speed == nil {
		b.Speed = map[string]*speed{}
	}
	if b.Accounts == nil {
		b.Accounts = map[string]*speed{}
	}
	if b.Usage == nil {
		b.Usage = map[string]*modelUse{}
	}
}

// entry returns m[k], creating it.
func entry[T any](m map[string]*T, k string) *T {
	v := m[k]
	if v == nil {
		v = new(T)
		m[k] = v
	}
	return v
}

func (b *bucket) add(o *bucket) {
	b.Used.add(o.Used)
	b.Requests += o.Requests
	b.WithTools += o.WithTools
	b.Trimmed += o.Trimmed
	b.Outputs += o.Outputs
	b.TrimmedOutputs += o.TrimmedOutputs
	b.CharsIn += o.CharsIn
	b.Saved += o.Saved
	b.NewSaved += o.NewSaved
	b.Would += o.Would
	b.NewWould += o.NewWould
	b.Kept += o.Kept
	b.KeptOutputs += o.KeptOutputs
	b.Skipped += o.Skipped
	b.Errors += o.Errors
	b.Unsafe += o.Unsafe
	for _, m := range []struct{ dst, src map[string]*counter }{{b.Rules, o.Rules}, {b.Models, o.Models}} {
		for k, c := range m.src {
			d := entry(m.dst, k)
			d.Hits += c.Hits
			d.Saved += c.Saved
			d.New += c.New
		}
	}
	for k, n := range o.Shadow {
		b.Shadow[k] += n
	}
	for k, t := range o.Tools {
		d := entry(b.Tools, k)
		d.Outputs += t.Outputs
		d.Chars += t.Chars
		d.Saved += t.Saved
	}
	for k, n := range o.Formats {
		b.Formats[k] += n
	}
	for _, m := range []struct{ dst, src map[string]*speed }{{b.Speed, o.Speed}, {b.Accounts, o.Accounts}} {
		for k, p := range m.src {
			entry(m.dst, k).add(*p)
		}
	}
	for k, u := range o.Usage {
		entry(b.Usage, k).add(*u)
	}
}

type recentTrim struct {
	At       int64    `json:"at"`
	Model    string   `json:"model"`
	Format   string   `json:"format"`
	Applied  bool     `json:"applied"`
	Outputs  int      `json:"outputs"`
	Trimmed  int      `json:"trimmed"`
	CharsIn  int      `json:"chars_in"`
	Saved    int      `json:"saved"`
	NewSaved int      `json:"new_saved"`
	Rules    []string `json:"rules"`
}

type persisted struct {
	Days    map[string]*bucket `json:"days"` // local date YYYY-MM-DD
	Recent  []recentTrim       `json:"recent"`
	Biggest []bigOutput        `json:"biggest"` // largest new tool outputs of the last 30 days
	Breaks  []cacheBreak       `json:"breaks"`  // latest cache breaks, oldest first
	Since   int64              `json:"since"`
}

type session struct {
	at     time.Time
	prompt int64
}

type cacheKey struct {
	key [32]byte
	m   mask
}

type service struct {
	mu         sync.Mutex
	cfg        settings
	configured bool
	lastConfig int64 // when the settings last changed; 0 = not since start
	data       persisted
	dirty      bool
	err        string
	pins       map[[32]byte]form
	pinLog     [][32]byte // insertion order of pins, oldest first
	pinPending []byte     // records not yet appended to pins.bin
	pinDir     string     // data dir the pins were loaded from
	sessions   map[string]session
	cache      map[cacheKey]cached
	stopCh     chan struct{}
	live       [liveMinutes]minute // ring indexed by minute number
	wg         sync.WaitGroup
	contextMu  sync.Mutex
	context    *contextService
}

type cached struct {
	text  string
	saved map[string]int
}

func newService() *service {
	return &service{cfg: defaultSettings(), data: persisted{Days: map[string]*bucket{}}, pins: map[[32]byte]form{},
		sessions: map[string]session{}, cache: map[cacheKey]cached{}}
}

var svc = newService()

func nowMs() int64 { return time.Now().UnixMilli() }

func (s *service) path() string { return filepath.Join(s.cfg.DataDir, statsFile) }

// configure applies settings, loads stats (and pins, when the data dir changed) and starts the flusher.
func (s *service) configure(cfg settings) {
	s.stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	if s.configured {
		s.lastConfig = nowMs()
	}
	s.configured = true
	s.err = ""
	s.data = persisted{Days: map[string]*bucket{}, Since: nowMs()}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		s.err = "data_dir: " + err.Error()
	} else {
		if raw, err := os.ReadFile(s.path()); err == nil {
			if json.Unmarshal(raw, &s.data) != nil || s.data.Days == nil {
				s.data = persisted{Days: map[string]*bucket{}, Since: nowMs()}
			}
		}
		if cfg.DataDir != s.pinDir {
			s.loadPinsLocked()
		}
	}
	s.pruneLocked(time.Now())
	s.stopCh = make(chan struct{})
	s.wg.Add(1)
	go s.flushLoop(s.stopCh)
	if err := s.startContext(cfg); err != nil {
		s.err = "archive unavailable: " + err.Error()
	}
}

func appendPin(buf []byte, k [32]byte, f form) []byte {
	return binary.LittleEndian.AppendUint16(append(buf, k[:]...), uint16(f))
}

// loadPinsLocked reads pins.bin. A file grown far past maxPins (old pins evicted) is rewritten compacted.
func (s *service) loadPinsLocked() {
	s.pins, s.pinLog, s.pinPending, s.pinDir = map[[32]byte]form{}, nil, nil, s.cfg.DataDir
	path := filepath.Join(s.pinDir, pinsFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	n := len(raw) / pinRecord
	for i := range n {
		rec := raw[i*pinRecord : (i+1)*pinRecord]
		s.pinLocked([32]byte(rec[:32]), form(binary.LittleEndian.Uint16(rec[32:])), false)
	}
	if n > 2*maxPins || len(raw)%pinRecord != 0 { // the remainder is a record cut short by a crash
		buf := make([]byte, 0, len(s.pinLog)*pinRecord)
		for _, k := range s.pinLog {
			buf = appendPin(buf, k, s.pins[k])
		}
		if err := writeAtomic(path, buf); err != nil {
			s.err = "data_dir: " + err.Error()
		}
	}
}

// pinLocked remembers the form k was first sent in; beyond maxPins the oldest pin is forgotten.
func (s *service) pinLocked(k [32]byte, f form, persist bool) {
	if _, ok := s.pins[k]; ok {
		return
	}
	s.pins[k] = f
	s.pinLog = append(s.pinLog, k)
	if len(s.pinLog) > maxPins {
		delete(s.pins, s.pinLog[0])
		s.pinLog = s.pinLog[1:]
	}
	if persist {
		s.pinPending = appendPin(s.pinPending, k, f)
	}
}

// pinned implements pinStore.
func (s *service) pinned(k [32]byte) (form, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.pins[k]
	return f, ok
}

func (s *service) stop() {
	s.stopContext()
	s.mu.Lock()
	ch := s.stopCh
	s.stopCh = nil
	s.mu.Unlock()
	if ch != nil {
		close(ch)
		s.wg.Wait()
	}
	s.flush()
}

func (s *service) flushLoop(stop chan struct{}) {
	defer s.wg.Done()
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.flush()
		}
	}
}

func (s *service) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.DataDir == "" {
		return
	}
	if len(s.pinPending) > 0 && s.pinDir == s.cfg.DataDir {
		if err := appendFile(filepath.Join(s.pinDir, pinsFile), s.pinPending); err != nil {
			s.err = "data_dir: " + err.Error()
		} else {
			s.pinPending = nil
		}
	}
	if !s.dirty {
		return
	}
	s.pruneLocked(time.Now())
	raw, err := json.Marshal(s.data)
	if err == nil {
		err = writeAtomic(s.path(), raw)
	}
	if err != nil {
		s.err = "data_dir: " + err.Error()
		return
	}
	s.err, s.dirty = "", false
}

func appendFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func (s *service) pruneLocked(now time.Time) {
	cut := now.AddDate(0, 0, -s.cfg.HistoryDays).Format(time.DateOnly)
	for day := range s.data.Days {
		if day < cut {
			delete(s.data.Days, day)
			s.dirty = true
		}
	}
}

// clean implements cleaner with a bounded cache keyed by content hash and rule mask.
func (s *service) clean(key [32]byte, m mask, text string) (string, map[string]int) {
	ck := cacheKey{key, m}
	s.mu.Lock()
	c, ok := s.cache[ck]
	s.mu.Unlock()
	if ok {
		return c.text, c.saved
	}
	out, saved := clean(text, m)
	s.mu.Lock()
	if len(s.cache) >= maxCache {
		clear(s.cache) // shortcut: wholesale reset instead of LRU; one re-clean per output afterwards
	}
	s.cache[ck] = cached{out, saved}
	s.mu.Unlock()
	return out, saved
}

// patch is request.patch_body_before: it receives the raw client request body and returns byte-range
// edits shortening tool outputs (none in measure mode). Any failure sends the body unchanged.
func (s *service) patch(body []byte) (out obj) {
	defer func() {
		if r := recover(); r != nil { // a panic inside the shared library would take the whole proxy down
			out = obj{}
			s.mu.Lock()
			s.err = fmt.Sprintf("trim panic: %v", r)
			s.mu.Unlock()
		}
	}()
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if cfg.Mode == modeOff {
		return obj{}
	}
	res, err := trim(body, cfg.rules(), s, s)
	s.record(res, err, cfg.Mode == modeApply)
	if err != nil || cfg.Mode != modeApply || len(res.Edits) == 0 {
		return obj{}
	}
	return obj{"edits": res.Edits}
}

// dayLocked returns the bucket of now's local day, creating it.
func (s *service) dayLocked(now time.Time) *bucket {
	day := now.Format(time.DateOnly)
	b := s.data.Days[day]
	if b == nil {
		b = newBucket()
		s.data.Days[day] = b
	}
	b.ensure()
	s.dirty = true
	return b
}

// usage is usage.handle: the host reports every finished request with the provider's token counts and
// its own timing.
func (s *service) usage(raw []byte) {
	var r struct {
		Model, Alias, SessionID, AuthID, AuthIndex string
		Failed                                     bool
		RequestedAt                                time.Time
		Latency, TTFT                              time.Duration // nanoseconds on the wire
		Detail                                     usageDetail
	}
	if json.Unmarshal(raw, &r) != nil {
		return
	}
	u := consumption(r.Detail, r.Failed)
	prompt := u.Input + u.CacheRead + u.CacheWrite
	p, timed := timing(u.Output, prompt, r.Latency, r.TTFT, r.Failed)
	model := firstNonEmpty(r.Model, firstNonEmpty(r.Alias, "unknown"))
	now := time.Now()
	started := r.RequestedAt
	if started.IsZero() {
		started = now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.dayLocked(now)
	b.Used.add(u)
	mu := entry(b.Usage, model)
	mu.used.add(u)
	if !r.Failed {
		s.watchCacheLocked(r.SessionID, model, prompt, u, started, mu)
	}
	m := now.Unix() / 60
	slot := &s.live[m%liveMinutes]
	if slot.At != m*60_000 {
		*slot = minute{At: m * 60_000}
	}
	slot.Tokens += u.Total
	slot.Requests++
	if !timed {
		return
	}
	slot.Speed.add(p)
	entry(b.Speed, model).add(p)
	entry(b.Accounts, firstNonEmpty(r.AuthID, firstNonEmpty(r.AuthIndex, "API key"))).add(p)
}

// watchCacheLocked spots a cached conversation being rebuilt: in the same session and model the prompt
// kept growing, yet most of the previous prompt was written to the cache again instead of read from it.
// Within the cache lifetime that is a break (something changed earlier history); after a longer pause the
// cache simply expired. Only providers that report cache writes (Anthropic) can show either.
func (s *service) watchCacheLocked(sessionID, model string, prompt int64, u used, started time.Time, mu *modelUse) {
	if sessionID == "" || prompt == 0 {
		return
	}
	k := sessionID + "\x00" + model
	prev, ok := s.sessions[k]
	s.sessions[k] = session{at: started, prompt: prompt}
	if len(s.sessions) > maxSessions {
		for key, v := range s.sessions {
			if started.Sub(v.at) > time.Hour {
				delete(s.sessions, key)
			}
		}
		if len(s.sessions) > maxSessions {
			clear(s.sessions)
		}
	}
	if !ok || prev.prompt < minCachePrompt || prompt < prev.prompt*9/10 || u.CacheRead >= prev.prompt/2 || u.CacheWrite < prev.prompt/2 {
		return
	}
	gap := started.Sub(prev.at)
	if gap > cacheTTL {
		mu.Expired++
		return
	}
	mu.Breaks++
	sum := sha256.Sum256([]byte(sessionID))
	s.data.Breaks = append(s.data.Breaks, cacheBreak{At: started.UnixMilli(), Model: model, Session: hex.EncodeToString(sum[:4]),
		Prompt: prompt, PrevPrompt: prev.prompt, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, GapS: int64(gap.Seconds()), Mode: s.cfg.Mode})
	if n := len(s.data.Breaks) - maxBreaks; n > 0 {
		s.data.Breaks = append([]cacheBreak(nil), s.data.Breaks[n:]...)
	}
}

// record folds one request into today's bucket, pins its new outputs and updates the recent lists.
func (s *service) record(res result, err error, applied bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.dayLocked(now)
	b.Requests++
	if err != nil {
		b.Errors++
		return
	}
	if res.Unsafe != "" {
		b.Unsafe++
		s.err = "patch check failed, request sent unchanged: " + res.Unsafe
	}
	if res.Outputs == 0 {
		return
	}
	b.WithTools++
	b.Outputs += int64(res.Outputs)
	b.CharsIn += int64(res.CharsIn)
	b.Kept += int64(res.Kept)
	b.KeptOutputs += int64(res.KeptOutputs)
	b.Skipped += int64(res.Skipped)
	b.Formats[firstNonEmpty(res.Format, "other")]++
	for name, n := range res.Shadow {
		b.Shadow[name] += int64(n)
	}
	model := firstNonEmpty(res.Model, "unknown")
	m := entry(b.Models, model)
	m.Hits++
	newSaved := 0
	for _, f := range res.fresh {
		s.pinLocked(f.key, f.form, true)
		newSaved += f.saved
		tool := firstNonEmpty(f.tool, "unknown")
		t := entry(b.Tools, tool)
		t.Outputs++
		t.Chars += int64(f.chars)
		t.Saved += int64(f.saved)
		s.noteBiggestLocked(bigOutput{At: now.UnixMilli(), Tool: tool, Model: model, Chars: f.chars, Saved: f.saved})
	}
	saved := res.Saved
	if !applied {
		saved = res.Would
	}
	if saved == 0 {
		return
	}
	newSaved = min(newSaved, saved)
	b.Trimmed++
	b.TrimmedOutputs += int64(res.Trimmed)
	m.Saved += int64(saved)
	m.New += int64(newSaved)
	if applied {
		b.Saved += int64(saved)
		b.NewSaved += int64(newSaved)
	} else {
		b.Would += int64(saved)
		b.NewWould += int64(newSaved)
	}
	names := make([]string, 0, len(res.Rules))
	for _, name := range ruleOrder {
		n, ok := res.Rules[name]
		if !ok {
			continue
		}
		names = append(names, name)
		c := entry(b.Rules, name)
		c.Hits++
		c.Saved += int64(n)
	}
	s.data.Recent = append(s.data.Recent, recentTrim{At: now.UnixMilli(), Model: model, Format: res.Format, Applied: applied,
		Outputs: res.Outputs, Trimmed: res.Trimmed, CharsIn: res.CharsIn, Saved: saved, NewSaved: newSaved, Rules: names})
	if n := len(s.data.Recent) - maxRecent; n > 0 {
		s.data.Recent = append([]recentTrim(nil), s.data.Recent[n:]...)
	}
}

// noteBiggestLocked keeps the largest new tool outputs of the last biggestWindow.
func (s *service) noteBiggestLocked(o bigOutput) {
	cut := o.At - biggestWindow.Milliseconds()
	keep := s.data.Biggest[:0]
	for _, b := range s.data.Biggest {
		if b.At >= cut {
			keep = append(keep, b)
		}
	}
	if len(keep) < maxBiggest {
		s.data.Biggest = append(keep, o)
		return
	}
	small := 0
	for i, b := range keep {
		if b.Chars < keep[small].Chars {
			small = i
		}
	}
	if o.Chars > keep[small].Chars {
		keep[small] = o
	}
	s.data.Biggest = keep
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- management routes ----

type statsView struct {
	Version     string               `json:"version"`
	Now         int64                `json:"now"`
	Since       int64                `json:"since"`
	LastConfig  int64                `json:"last_config"` // when the settings last changed, 0 = not since start
	Settings    settingsView         `json:"settings"`
	Error       string               `json:"error"`
	Days        []dayView            `json:"days"` // oldest first, every day of the range
	Total       *bucket              `json:"total"`
	Today       *bucket              `json:"today"`
	Month       used                 `json:"month"`       // consumption since the 1st of this month
	MonthUsage  map[string]*modelUse `json:"month_usage"` // the same by model, for cost
	Live        []minute             `json:"live"`        // consumption per minute, last hour, oldest first
	Recent      []recentTrim         `json:"recent"`
	Biggest     []bigOutput          `json:"biggest"` // largest first
	Breaks      []cacheBreak         `json:"breaks"`  // newest first
	Pins        int                  `json:"pins"`    // tool outputs whose first-sent form is remembered
	MaxPins     int                  `json:"max_pins"`
	RateEdges   []float64            `json:"rate_edges"`
	TTFTEdges   []float64            `json:"ttft_edges"`
	PromptEdges []float64            `json:"prompt_edges"`
}

// dayView is one day of the per-day chart.
type dayView struct {
	Day      string `json:"day"`
	Saved    int64  `json:"saved"`
	NewSaved int64  `json:"new_saved"`
	Would    int64  `json:"would"`
	NewWould int64  `json:"new_would"`
}

type settingsView struct {
	Mode            string           `json:"mode"`
	Rules           map[string]bool  `json:"rules"`
	MinChars        int              `json:"min_output_chars"`
	DedupeMin       int              `json:"dedupe_min_chars"`
	SkipTools       []string         `json:"skip_tools"`
	SkipModels      []string         `json:"skip_models"`
	Prices          map[string]price `json:"prices"`
	ArchiveEnabled  bool             `json:"archive_enabled"`
	ArchiveListen   string           `json:"archive_listen"`
	ArchiveMaxBytes int64            `json:"archive_max_bytes"`
	HistoryBudget   int              `json:"history_budget_bytes"`
	HistoryRecent   int              `json:"history_recent_bytes"`
	OutputLimit     int              `json:"output_limit_bytes"`
}

func (s *service) stats(days int) statsView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	c := s.cfg
	v := statsView{Version: version, Now: now.UnixMilli(), Since: s.data.Since, LastConfig: s.lastConfig, Error: s.err,
		Total: newBucket(), Today: newBucket(), MonthUsage: map[string]*modelUse{}, Pins: len(s.pins), MaxPins: maxPins,
		RateEdges: rateEdges[:], TTFTEdges: ttftEdges[:], PromptEdges: promptEdges[:],
		Settings: settingsView{Mode: c.Mode, Rules: c.switches(), MinChars: c.MinChars, DedupeMin: c.DedupeMin,
			SkipTools: append([]string{}, c.SkipTools...), SkipModels: append([]string{}, c.SkipModels...), Prices: map[string]price{},
			ArchiveEnabled: c.ArchiveEnabled, ArchiveListen: c.ArchiveListen, ArchiveMaxBytes: c.ArchiveMaxBytes,
			HistoryBudget: c.HistoryBudget, HistoryRecent: c.HistoryRecent, OutputLimit: c.OutputLimit}}
	for k, p := range c.Prices {
		v.Settings.Prices[k] = p
	}
	for i := days - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format(time.DateOnly)
		dv := dayView{Day: day}
		if d := s.data.Days[day]; d != nil {
			v.Total.add(d)
			if i == 0 {
				v.Today.add(d)
			}
			dv.Saved, dv.NewSaved, dv.Would, dv.NewWould = d.Saved, d.NewSaved, d.Would, d.NewWould
		}
		v.Days = append(v.Days, dv)
	}
	month := now.Format("2006-01")
	for day, b := range s.data.Days {
		if strings.HasPrefix(day, month) {
			v.Month.add(b.Used)
			for k, u := range b.Usage {
				entry(v.MonthUsage, k).add(*u)
			}
		}
	}
	cur := now.Unix() / 60
	for m := cur - liveMinutes + 1; m <= cur; m++ {
		slot := minute{At: m * 60_000}
		if s.live[m%liveMinutes].At == slot.At {
			slot = s.live[m%liveMinutes]
		}
		v.Live = append(v.Live, slot)
	}
	v.Recent = append([]recentTrim{}, s.data.Recent...)
	sort.Slice(v.Recent, func(i, j int) bool { return v.Recent[i].At > v.Recent[j].At })
	cut := now.Add(-biggestWindow).UnixMilli()
	for _, b := range s.data.Biggest {
		if b.At >= cut {
			v.Biggest = append(v.Biggest, b)
		}
	}
	sort.Slice(v.Biggest, func(i, j int) bool { return v.Biggest[i].Chars > v.Biggest[j].Chars })
	for i := len(s.data.Breaks) - 1; i >= 0; i-- {
		v.Breaks = append(v.Breaks, s.data.Breaks[i])
	}
	return v
}

// reset clears the trim statistics. Consumption, speed and cache health are kept, and so are the pins:
// they protect conversations that are still open.
func (s *service) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	days := map[string]*bucket{}
	for day, b := range s.data.Days {
		nb := newBucket()
		nb.Used, nb.Speed, nb.Accounts, nb.Usage = b.Used, b.Speed, b.Accounts, b.Usage
		nb.ensure()
		days[day] = nb
	}
	s.data = persisted{Days: days, Breaks: s.data.Breaks, Since: nowMs()}
	s.dirty = true
}

type managementRequest struct {
	Method string
	Path   string
	Query  map[string][]string
	Body   []byte
}

type obj = map[string]any

func jsonResponse(status int, v any) obj {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"encode failed"}`)
	}
	return obj{"StatusCode": status, "Headers": map[string][]string{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, "Body": body}
}

func (s *service) handleManagement(r managementRequest) obj {
	i := strings.LastIndex(r.Path, "/token-trim/")
	if i < 0 {
		return jsonResponse(http.StatusNotFound, obj{"error": "not found"})
	}
	route := r.Path[i+len("/token-trim/"):]
	if strings.HasPrefix(route, "archive/") {
		return s.manageArchive(route, r)
	}
	switch {
	case route == "dashboard" && r.Method == http.MethodGet:
		return obj{"StatusCode": 200, "Headers": map[string][]string{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}}, "Body": page}
	case route == "stats" && r.Method == http.MethodGet:
		days := 30
		if v := r.Query["days"]; len(v) > 0 {
			if n, err := strconv.Atoi(v[0]); err == nil {
				days = min(max(n, 1), 400)
			}
		}
		return jsonResponse(http.StatusOK, s.stats(days))
	case route == "preview" && r.Method == http.MethodPost:
		var in struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(r.Body, &in) != nil {
			return jsonResponse(http.StatusBadRequest, obj{"error": `body must be {"text":"…"}`})
		}
		s.mu.Lock()
		rl := s.cfg.rules()
		s.mu.Unlock()
		out, saved := clean(in.Text, rl.On&textBits)
		return jsonResponse(http.StatusOK, obj{"text": out, "before": len(in.Text), "after": len(out), "rules": saved})
	case route == "reset" && r.Method == http.MethodPost:
		s.reset()
		return jsonResponse(http.StatusOK, s.stats(30))
	}
	return jsonResponse(http.StatusNotFound, obj{"error": "not found"})
}

// ---- plugin methods ----

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// logoURI: scissors on a teal badge, shown by CPAMC in the sidebar at 16 px.
var logoURI = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">`+
	`<defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#14B8A6"/><stop offset="1" stop-color="#0E7490"/></linearGradient></defs>`+
	`<rect width="24" height="24" rx="6" fill="url(#g)"/>`+
	`<g fill="none" stroke="#fff" stroke-width="1.9" stroke-linecap="round"><circle cx="7.6" cy="16.4" r="2.4"/><circle cx="16.4" cy="16.4" r="2.4"/>`+
	`<path d="M9.3 14.7 17.5 5.5M14.7 14.7 6.5 5.5"/></g></svg>`))

func handleMethod(method string, req []byte) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = errorEnvelope("plugin_panic", fmt.Sprint(r)), nil
		}
	}()
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var lc struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(req) > 0 {
			if err := json.Unmarshal(req, &lc); err != nil {
				return nil, err
			}
		}
		cfg, err := parseSettings(lc.ConfigYAML)
		if err != nil {
			return nil, err
		}
		svc.configure(cfg)
		return okEnvelope(obj{
			"schema_version": schemaVersion,
			"metadata": obj{
				"Name": "token-trim", "Version": version, "Author": "NewWay",
				"GitHubRepository": "local", // required non-empty by the host; display only
				"Logo":             logoURI,
				"ConfigFields":     configFields(),
			},
			"capabilities": map[string]bool{"management_api": true, "request_interceptor": true, "request_interceptor_omit_body_after": true, "usage_plugin": true},
		})
	case "plugin.quiesce", "plugin.shutdown":
		svc.stop()
		return okEnvelope(obj{})
	case "management.register":
		return okEnvelope(obj{
			"routes": []map[string]string{
				{"Method": "GET", "Path": "token-trim/stats"},
				{"Method": "POST", "Path": "token-trim/preview"},
				{"Method": "POST", "Path": "token-trim/reset"},
				{"Method": "GET", "Path": "token-trim/archive/namespaces"},
				{"Method": "POST", "Path": "token-trim/archive/token"},
				{"Method": "POST", "Path": "token-trim/archive/revoke"},
			},
			"resources": []map[string]string{{
				"Path": "/dashboard", "Menu": "Token Trim",
				"Description": "Trims tool outputs (colour codes, progress bars, repeated log lines, JSON whitespace, duplicates) before they reach the model.",
			}},
		})
	case "management.handle":
		var r managementRequest
		if err := json.Unmarshal(req, &r); err != nil {
			return nil, err
		}
		return okEnvelope(svc.handleManagement(r))
	case "request.intercept_before":
		return okEnvelope(svc.intercept(req))
	case "request.intercept_after":
		return okEnvelope(obj{})
	case "usage.handle":
		svc.usage(req)
		return okEnvelope(obj{})
	}
	return errorEnvelope("unknown_method", "unknown method: "+method), nil
}
