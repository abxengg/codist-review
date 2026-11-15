package activity

import (
	"strings"
	"testing"
	"time"
)

// A frame shows the lanes, the call under way with its stage, phase and tokens, totals, recent calls and the log,
// and never runs wider or taller than the terminal.
func TestFrame(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	s := Snapshot{Now: now}
	s.Status.Repo, s.Status.Branch, s.Status.Model = "maze-lab", "main", "qwen3.5:9b"
	s.Lanes = []LaneView{{Name: "Code checks"}, {Name: "AI model", Running: "c2a0d45abcdef", Since: now.Add(-90 * time.Second), Count: 4}}
	run := Call{ID: "9", At: now.Add(-40 * time.Second).UnixMilli(), Kind: "review", Title: "Review GameState.java", Status: "running", SHA: "c2a0d45abc",
		Path: "src/GameState.java", Phase: "writing", FirstTokenMs: 30000, ReplyTokens: 250, System: strings.Repeat("s", 4000), User: strings.Repeat("u", 8000),
		Response: "{\n \"change\": \"adds turnBuffer--\",\n \"effect\": \"the buffer runs out after 4 steps\""}
	s.Running = &run
	s.Calls = []Call{run, {ID: "8", At: now.Add(-5 * time.Minute).UnixMilli(), Kind: "second look, choice", Status: "done", PromptTokens: 3100, ReplyTokens: 60,
		TookMs: 9000, PrefillMs: 1000, DecodeMs: 6000}, {ID: "7", Kind: "review", Status: "failed", PromptTokens: 3900, ReplyTokens: 2500, TookMs: 200000, DecodeMs: 190000}}
	s.Log = []Entry{{At: now, Level: "WARN", Msg: "model wrote only blank space; call ended", Attrs: map[string]string{"model": "qwen3.5:9b"}},
		{At: now, Level: "DEBUG", Msg: "request", Attrs: map[string]string{"path": "/api/activity/lanes"}}}
	f := Frame(s, 100, 60)
	for _, want := range []string{"CODIST", "maze-lab", "AI model", "c2a0d45", "REVIEW", "WRITING", "≈3,000 tokens in", "250", "10% of 2,500",
		"the buffer runs out", "calls ", "3,100 → 60", "second look, choice", "blank space", "q quit"} {
		if !strings.Contains(f, want) {
			t.Errorf("frame lacks %q:\n%s", want, f)
		}
	}
	if strings.Contains(f, "/api/activity/lanes") {
		t.Error("page requests are left out of the log")
	}
	rows := strings.Split(f, "\n")
	for _, r := range rows {
		if visible(r) > 100 {
			t.Errorf("row of %d columns: %q", visible(r), r)
		}
	}
	if small := strings.Split(Frame(s, 100, 12), "\n"); len(small) > 12 {
		t.Errorf("%d rows in a 12-row terminal", len(small))
	}
	s.Running.Phase, s.Running.ReplyTokens = "reading", 0
	if f := Frame(s, 100, 60); !strings.Contains(f, "READING THE PROMPT") {
		t.Errorf("reading phase:\n%s", f)
	}
	if f := Frame(Snapshot{Now: now, Err: "Codist is not answering"}, 80, 20); !strings.Contains(f, "not answering") {
		t.Error(f)
	}
	if num(1234567) != "1,234,567" || num(12) != "12" || num(-1234) != "-1,234" {
		t.Errorf("%s %s %s", num(1234567), num(12), num(-1234))
	}
}

// The dashboard says in plain words what is happening, on which step, how far it has got and what each commit brought.
func TestDashboard(t *testing.T) {
	now := time.Date(2026, 11, 15, 15, 0, 0, 0, time.Local)
	s := Snapshot{Now: now}
	s.Status.Repo, s.Status.Branch, s.Status.Model = "demo-shop", "main", "qwen2.5-coder:14b"
	s.Lanes = []LaneView{{Name: "Code checks"}, {Name: "AI model", Running: "aaa111", Count: 2, Finished: []Finished{{TookMs: 60000}}}, {Name: "Behaviour runs"}}
	row := func(sha, msg, status, model string, counts map[string]int) CommitRow {
		var r CommitRow
		r.Commit.SHA, r.Commit.Message, r.Status, r.Model, r.Counts = sha, msg, status, model, counts
		return r
	}
	s.Commits = []CommitRow{row("aaa111", "List a customer's orders", "done", "", nil), row("bbb222", "Let customers cancel an order", "done", "m", map[string]int{"medium": 1}),
		row("ccc333", "Simplify the stock check", "done", "m", map[string]int{"high": 1}), row("ddd444", "Look up products", "done", "m", nil)}
	s.Issues = []IssueRow{{Title: "Exception swallowed by an empty catch block", Path: "src/OrderService.java", Line: 40, FirstSeen: "bbb222"}}
	run := Call{ID: "1", At: now.Add(-20 * time.Second).UnixMilli(), Kind: "second look, choice", Title: "Last item cannot be bought", SHA: "aaa111",
		Path: "src/Product.java", Status: "running", Phase: "writing", ReplyTokens: 40, System: strings.Repeat("x", 5000), User: strings.Repeat("y", 5000)}
	s.Running = &run
	s.Calls = []Call{run, {Status: "done", TookMs: 60000, PrefillMs: 30000, DecodeMs: 30000, ReplyTokens: 300}}
	f := Dashboard(s, 120, 60)
	for _, want := range []string{"watching", "demo-shop", "NOW", "double-checking a problem", "List a customer's orders", "Product.java",
		"writing its answer", "HOW A COMMIT IS CHECKED", "◉ AI double-checks", "✓ AI reads it", "3 of 6 commits", "about 3m00s left",
		"2 problems", "1 serious", "1 clean commit", "Exception swallowed", "the AI is on it now", "no problems found", "50% of that time", "d technical details"} {
		if !strings.Contains(f, want) {
			t.Errorf("dashboard lacks %q:\n%s", want, f)
		}
	}
	for _, r := range strings.Split(f, "\n") {
		if visible(r) > 120 {
			t.Errorf("row of %d columns: %q", visible(r), r)
		}
	}
	s.Running, s.Lanes = nil, nil
	if f := Dashboard(s, 100, 40); !strings.Contains(f, "All caught up") || !strings.Contains(f, "✓ Result") {
		t.Errorf("idle:\n%s", f)
	}
}

// The dashboard reads the AI's thinking out of a reply that is still being written, and says why it decided.
func TestDashboardThinking(t *testing.T) {
	now := time.Date(2026, 11, 15, 15, 0, 0, 0, time.Local)
	s := Snapshot{Now: now}
	s.Status.Repo = "demo-shop"
	s.Running = &Call{Kind: "review", Status: "running", Phase: "writing", ReplyTokens: 30, At: now.UnixMilli(), SHA: "af34e37", Path: "src/Product.java",
		System: "You are a reviewer.\\n## This team's rules\\nStock never goes below zero.", User: "## Commit\\nSimplify\\n## Diff of x\\n```diff\\n```\\n## Product.java after the commit\\n",
		Response: `{"change": "The check now refuses an order that leaves 0 in stock.", "effect": "The last item cannot be bought", "expected": "rule 3 says stock may reach `}
	s.LastRead = &Call{Kind: "review", SHA: "b8c3e17", Path: "src/OrderService.java", Response: `{"change":"adds cancel","effect":"a shipped order cancels silently","expected":"no rule","findings":[{"title":"Failed cancel is hidden"}]}`}
	s.LastJudge = &Call{Kind: "second look, choice", Title: "Failed cancel is hidden", SHA: "b8c3e17", Path: "src/OrderService.java",
		User: "## The disagreement\\nReviewer A says: Failed cancel is hidden\\nReviewer B says: The code after the commit is correct here.", Response: `{"reason":"line 40 drops the error","right":"A"}`}
	f := Dashboard(s, 140, 80)
	for _, want := range []string{"WHAT THE AI IS THINKING", "the commit message, the change (diff), the code after the change, your team's rules",
		"refuses an order that leaves 0", "The last item cannot be bought", "rule 3 says stock may reach", "LAST FILE IT READ", "Failed cancel is hidden",
		"LAST DOUBLE-CHECK", "kept the problem", "line 40 drops the error"} {
		if !strings.Contains(f, want) {
			t.Errorf("lacks %q:\n%s", want, f)
		}
	}
}
