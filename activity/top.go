package activity

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Top is a live view of a running Codist in the terminal. It opens on a dashboard written for anyone watching: what
// Codist is doing now in plain words, the steps a commit goes through, how far it has got, and what it found in each
// recent commit. d switches to the technical view: lanes, the model call with its tokens, recent calls and the log.
// It reads the same pages the Activity screen reads, twice a second, until q or Ctrl+C.
func Top(base string, out io.Writer) error {
	base = strings.TrimRight(base, "/")
	if _, err := fetch(base, "/api/status", new(map[string]any)); err != nil {
		return fmt.Errorf("no Codist at %s (%v); start it first, or name its address: codist top http://localhost:8080", base, err)
	}
	restore := rawKeys()
	defer restore()
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l") // the terminal's second screen, cursor hidden
	defer fmt.Fprint(out, "\x1b[?25h\x1b[?1049l")
	quit := make(chan struct{})
	go func() { // q quits; so does Ctrl+C
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		keys := make(chan byte)
		go func() {
			b := make([]byte, 1)
			for {
				if n, err := os.Stdin.Read(b); err != nil {
					return
				} else if n == 1 {
					keys <- b[0]
				}
			}
		}()
		for {
			select {
			case <-sig:
				close(quit)
				return
			case k := <-keys:
				if k == 'q' || k == 'Q' || k == 3 {
					close(quit)
					return
				}
				if k == 'd' || k == 'D' {
					details.Store(!details.Load())
				}
			}
		}
	}()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		w, h := termSize()
		draw := Dashboard
		if details.Load() {
			draw = Frame
		}
		// Each row clears the rest of its line, so nothing of a wider earlier frame is left behind.
		fmt.Fprint(out, "\x1b[H"+strings.ReplaceAll(draw(snapshotOf(base), w, h), "\n", "\x1b[K\n")+"\x1b[K\x1b[J")
		select {
		case <-quit:
			return nil
		case <-tick.C:
		}
	}
}

// details is whether the technical view is shown in place of the dashboard; d switches.
var details atomic.Bool

// Snapshot is what one frame shows.
type Snapshot struct {
	Now    time.Time
	Status struct {
		Repo, Branch, Model, LLMProblem string
		Path                            string
	}
	Lanes     []LaneView
	Calls     []Call // newest first, without texts
	Running   *Call  // the call under way, with its prompt and reply so far
	Log       []Entry
	Err       string
	LastRead  *Call       // the newest finished reading of a file, with its prompt and reply
	LastJudge *Call       // the newest finished check of a claim, with its prompt and reply
	Commits   []CommitRow // newest first
	Issues    []IssueRow  // open issues
}

// CommitRow is a commit as the dashboard lists it.
type CommitRow struct {
	Commit struct {
		SHA     string `json:"sha"`
		Short   string `json:"shortSha"`
		Message string `json:"message"`
	} `json:"commit"`
	Status    string         `json:"status"` // of the code checks: queued, running, done
	Model     string         `json:"model"`  // set once the model has read it
	Counts    map[string]int `json:"counts"` // issues it brought, by severity
	Fixed     int            `json:"fixed"`
	Behaviour string         `json:"behaviour"`
}

// IssueRow is an open issue as the dashboard names it.
type IssueRow struct {
	Title, Path, Severity, Source, FirstSeen string
	Line                                     int
}

func snapshotOf(base string) Snapshot {
	s := Snapshot{Now: time.Now()}
	var st struct {
		Repo, Branch, Model, Path string
		LLMProblem                string `json:"llmProblem"`
	}
	if _, err := fetch(base, "/api/status", &st); err != nil {
		s.Err = "Codist is not answering: " + err.Error()
		return s
	}
	s.Status.Repo, s.Status.Branch, s.Status.Model, s.Status.LLMProblem, s.Status.Path = st.Repo, st.Branch, st.Model, st.LLMProblem, st.Path
	_, _ = fetch(base, "/api/activity/lanes", &s.Lanes)
	_, _ = fetch(base, "/api/activity", &s.Calls)
	for _, c := range s.Calls { // newest first: the last finished reading and the last finished check of a claim
		if c.Status != "done" || (s.LastRead != nil || c.Kind != "review") && (s.LastJudge != nil || !strings.HasPrefix(c.Kind, "second look")) {
			continue
		}
		var full Call
		if _, err := fetch(base, "/api/activity/"+c.ID, &full); err == nil {
			if c.Kind == "review" {
				s.LastRead = &full
			} else {
				s.LastJudge = &full
			}
		}
		if s.LastRead != nil && s.LastJudge != nil {
			break
		}
	}
	for _, c := range s.Calls {
		if c.Status == "running" {
			var full Call
			if _, err := fetch(base, "/api/activity/"+c.ID, &full); err == nil {
				s.Running = &full
			}
			break
		}
	}
	var lines []Entry
	if _, err := fetch(base, "/api/activity/log", &lines); err == nil {
		s.Log = lines
	}
	_, _ = fetch(base, "/api/commits", &s.Commits)
	var is struct{ Issues []IssueRow }
	if _, err := fetch(base, "/api/issues", &is); err == nil {
		s.Issues = is.Issues
	}
	return s
}

func fetch(base, path string, into any) (int, error) {
	c := http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("%s", resp.Status)
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(into)
}

// ---------------------------------------------------------------- drawing

const (
	reset, bold, dim                        = "\x1b[0m", "\x1b[1m", "\x1b[2m"
	red, green, yellow, blue, magenta, cyan = "\x1b[31m", "\x1b[32m", "\x1b[33m", "\x1b[34m", "\x1b[35m", "\x1b[36m"
	maxReply                                = 2500 // the reply limit Codist sets
)

// Frame draws one screen of w columns and at most h rows. It is a pure function of the snapshot, so it can be tested.
func Frame(s Snapshot, w, h int) string {
	w = max(60, w)
	var rows []string
	add := func(r ...string) { rows = append(rows, r...) }

	clock := s.Now.Format("15:04:05")
	title := bold + cyan + " ◆ CODIST" + reset + dim + "  live" + reset
	if s.Err != "" {
		add(line(title, clock, w), "", " "+red+s.Err+reset, "", dim+" q quit"+reset)
		return strings.Join(fit(rows, h), "\n")
	}
	model := green + "●" + reset + " " + s.Status.Model
	if s.Status.LLMProblem != "" {
		model = red + "●" + reset + " " + s.Status.Model + dim + "  " + s.Status.LLMProblem + reset
	}
	add(line(title+"   "+bold+s.Status.Repo+reset+dim+" · "+s.Status.Branch+reset, model+"   "+dim+clock+reset, w))

	// Lanes.
	var lane []string
	for _, l := range s.Lanes {
		state := green + "● idle" + reset
		if l.Running != "" {
			state = yellow + "◉ " + short(l.Running) + reset + " " + dim + ago(s.Now, l.Since) + reset
		}
		lane = append(lane, line(" "+pad(l.Name, 16)+state, dim+fmt.Sprintf("waiting %-4d done %d", l.Count, len(l.Finished))+reset+" ", w-2))
	}
	add(box("Lanes", lane, w)...)

	// The call under way.
	var now []string
	if c := s.Running; c != nil {
		elapsed := s.Now.Sub(time.UnixMilli(c.At))
		where := strings.TrimSpace(short(c.SHA) + "  " + base(c.Path))
		now = append(now, " "+dim+"stage "+reset+magenta+bold+strings.ToUpper(c.Kind)+reset+"  "+where+dim+"  ·  "+c.Title+reset)
		promptTok := (len(c.System) + len(c.User)) / 4
		if c.Phase == "reading" || c.ReplyTokens == 0 {
			now = append(now, " "+dim+"phase "+reset+yellow+bold+"READING THE PROMPT"+reset+fmt.Sprintf("  ≈%s tokens in  ·  %s so far", num(promptTok), dur(elapsed)))
			now = append(now, " "+dim+"      the model reads the whole prompt before it writes a word; a prompt that starts like the last one is read in a second"+reset)
		} else {
			writing := elapsed - time.Duration(c.FirstTokenMs)*time.Millisecond
			rate := 0.0
			if writing > 0 {
				rate = float64(c.ReplyTokens) / writing.Seconds()
			}
			now = append(now, " "+dim+"phase "+reset+green+bold+"WRITING"+reset+fmt.Sprintf("  ≈%s tokens in, read in %s", num(promptTok), dur(time.Duration(c.FirstTokenMs)*time.Millisecond)))
			now = append(now, " "+dim+"out   "+reset+bold+num(c.ReplyTokens)+reset+" tokens  "+bar(float64(c.ReplyTokens)/maxReply, 24)+
				fmt.Sprintf(" %d%% of %s   %s%.1f tok/s%s", c.ReplyTokens*100/maxReply, num(maxReply), cyan, rate, reset))
		}
		if tail := lastLines(c.Response, 3, w-8); len(tail) > 0 {
			for _, t := range tail {
				now = append(now, " "+dim+"┊ "+reset+t)
			}
		}
	} else {
		now = append(now, " "+dim+"The model is not working on anything."+reset)
	}
	add(box("Model now", now, w)...)

	// Totals over the calls kept, and a line of speeds.
	var in, outTok, failed int
	var took, prefill, decode int64
	var speeds []float64
	done := 0
	for i := len(s.Calls) - 1; i >= 0; i-- {
		c := s.Calls[i]
		if c.Status == "running" {
			continue
		}
		done++
		in += c.PromptTokens
		outTok += c.ReplyTokens
		took += c.TookMs
		prefill += c.PrefillMs
		decode += c.DecodeMs
		if c.Status == "failed" {
			failed++
		}
		if c.DecodeMs > 0 {
			speeds = append(speeds, float64(c.ReplyTokens)/(float64(c.DecodeMs)/1000))
		}
	}
	kinds := map[string]int{}
	for _, c := range s.Calls {
		kinds[c.Kind]++
	}
	var ks []string
	for k, n := range kinds {
		ks = append(ks, fmt.Sprintf("%s %d", k, n))
	}
	sort.Strings(ks)
	tot := []string{
		fmt.Sprintf(" calls %s%d%s  failed %s  tokens in %s%s%s  out %s%s%s  model time %s  (reading %s, writing %s)",
			bold, done, reset, map[bool]string{true: red + strconv.Itoa(failed) + reset, false: "0"}[failed > 0], bold, num(in), reset, bold, num(outTok), reset,
			dur(time.Duration(took)*time.Millisecond), dur(time.Duration(prefill)*time.Millisecond), dur(time.Duration(decode)*time.Millisecond)),
		" " + dim + "stages  " + reset + strings.Join(ks, dim+" · "+reset),
	}
	if len(speeds) > 0 {
		tot = append(tot, " "+dim+"tok/s   "+reset+cyan+spark(speeds, w-20)+reset+dim+fmt.Sprintf("  last %.1f", speeds[len(speeds)-1])+reset)
	}
	add(box("Totals", tot, w)...)

	// Recent calls.
	var rec []string
	for _, c := range s.Calls {
		if len(rec) == 8 {
			break
		}
		mark := map[string]string{"done": green + "✓" + reset, "failed": red + "✗" + reset, "running": yellow + "…" + reset}[c.Status]
		at := time.UnixMilli(c.At).Format("15:04:05")
		rec = append(rec, fmt.Sprintf(" %s %s%s%s %s %s %s %s",
			mark, dim, at, reset, pad(c.Kind, 22), pad(short(c.SHA)+" "+base(c.Path), 26),
			pad(fmt.Sprintf("%s → %s", num(c.PromptTokens), num(c.ReplyTokens)), 15), dim+dur(time.Duration(c.TookMs)*time.Millisecond)+reset))
	}
	if len(rec) == 0 {
		rec = append(rec, " "+dim+"No model calls yet."+reset)
	}
	add(box("Recent calls   in → out tokens", rec, w)...)

	// The log, as much as fits.
	left := h - len(rows) - 3
	var keep []Entry // git commands and page requests (this view's own among them) are noise here
	for _, l := range s.Log {
		if l.Msg != "git" && l.Msg != "request" {
			keep = append(keep, l)
		}
	}
	var logs []string
	for i := max(0, len(keep)-max(0, left)); i < len(keep); i++ {
		l := keep[i]
		colour := dim
		switch l.Level {
		case "WARN":
			colour = yellow
		case "ERROR":
			colour = red
		}
		at := l.At.Format("15:04:05")
		logs = append(logs, " "+dim+at+reset+" "+colour+l.Msg+reset+" "+dim+attrs(l.Attrs)+reset)
	}
	if left > 0 && len(logs) > 0 {
		add(box("Log", logs, w)...)
	}
	add(dim + " d back to the dashboard · q quit · refreshes twice a second" + reset)
	return strings.Join(fit(rows, h), "\n")
}

func box(title string, body []string, w int) []string {
	out := []string{dim + "╭─ " + reset + bold + title + reset + dim + " " + strings.Repeat("─", max(0, w-6-visible(title))) + "╮" + reset}
	for _, b := range body {
		out = append(out, dim+"│"+reset+pad(cut(b, w-2), w-2)+dim+"│"+reset)
	}
	return append(out, dim+"╰"+strings.Repeat("─", w-2)+"╯"+reset)
}

// line puts left and right on one row of width w.
func line(left, right string, w int) string {
	gap := w - visible(left) - visible(right)
	if gap < 1 {
		return cut(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

func bar(f float64, n int) string {
	f = min(1, max(0, f))
	full := int(f * float64(n))
	return green + strings.Repeat("█", full) + reset + dim + strings.Repeat("░", n-full) + reset
}

func spark(v []float64, n int) string {
	if len(v) > n {
		v = v[len(v)-n:]
	}
	hi := 0.0
	for _, x := range v {
		hi = max(hi, x)
	}
	ticks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, x := range v {
		i := 0
		if hi > 0 {
			i = int(x / hi * float64(len(ticks)-1))
		}
		b.WriteRune(ticks[i])
	}
	return b.String()
}

func lastLines(s string, n, w int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, cut(l, w))
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func attrs(a map[string]string) string {
	var ks []string
	for k := range a {
		if k != "lib" {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	var b strings.Builder
	for _, k := range ks {
		fmt.Fprintf(&b, "%s=%s ", k, a[k])
	}
	return b.String()
}

// visible is the width of s on screen, without colour codes.
func visible(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case r == '\x1b':
			esc = true
		case esc:
			if r == 'm' {
				esc = false
			}
		default:
			n++
		}
	}
	return n
}

// cut shortens s to w columns, keeping its colour codes.
func cut(s string, w int) string {
	if visible(s) <= w {
		return s
	}
	var b strings.Builder
	n, esc := 0, false
	for _, r := range s {
		switch {
		case r == '\x1b':
			esc = true
			b.WriteRune(r)
		case esc:
			b.WriteRune(r)
			if r == 'm' {
				esc = false
			}
		case n < w-1:
			b.WriteRune(r)
			n++
		}
	}
	return b.String() + "…" + reset
}

func pad(s string, w int) string { return s + strings.Repeat(" ", max(0, w-visible(s))) }

func fit(rows []string, h int) []string {
	if h > 0 && len(rows) > h {
		rows = rows[:h]
	}
	return rows
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func base(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func num(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && !(i == 1 && s[0] == '-'); i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func dur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return dur(now.Sub(t))
}

// ---------------------------------------------------------------- the terminal

// termSize asks the terminal for its size, or assumes 120 by 40.
func termSize() (w, h int) {
	w, h = 120, 40
	if runtime.GOOS == "windows" {
		return
	}
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	if out, err := cmd.Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) == 2 {
			h, _ = strconv.Atoi(f[0])
			w, _ = strconv.Atoi(f[1])
		}
	}
	return max(60, w), max(20, h)
}

// rawKeys lets single key presses through without Enter, and returns what puts the terminal back.
func rawKeys() func() {
	if runtime.GOOS == "windows" {
		return func() {}
	}
	save := exec.Command("stty", "-g")
	save.Stdin = os.Stdin
	was, err := save.Output()
	if err != nil {
		return func() {}
	}
	set := exec.Command("stty", "-icanon", "-echo", "min", "1")
	set.Stdin = os.Stdin
	_ = set.Run()
	return func() {
		back := exec.Command("stty", strings.TrimSpace(string(was)))
		back.Stdin = os.Stdin
		_ = back.Run()
	}
}
