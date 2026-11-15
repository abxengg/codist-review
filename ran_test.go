package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abxengg/codist-git/testrepo"
	"github.com/abxengg/codist-review/store"
)

// A commit that says it simplifies but changes results is reported without a model, whichever of the review and
// the run comes first; the model is shown the changed results of the file it reads.
func TestWhatRunningShowed(t *testing.T) {
	tr, r, st := setup(t)
	tr.Commit("Simplify the discount", map[string]string{testrepo.ServicePath: strings.Replace(testrepo.ServiceFix, "order.applyDiscount();", "order.applyDiscount(); order.applyDiscount();", 1)})
	sha := tr.Head()
	report := json.RawMessage(`{"sha":"` + sha + `","status":"done","differ":2,"differences":[` +
		`{"method":"com.shop.order.OrderService#discount","call":"discount(Order(100))","old":"90 | args [x]","new":"81 | args [x]"},` +
		`{"method":"com.shop.order.Other#f","call":"f()","old":"1","new":"2"}]}`)
	// The run finishes first: the review brings the finding.
	st.SetBehaviour(sha, report)
	fm := &fakeModel{reply: `{"findings":[]}`}
	r.Model = fm.client(t)
	if err := r.Review(context.Background(), sha, Options{}); err != nil {
		t.Fatal(err)
	}
	fs := st.Findings(sha)
	if len(fs) != 1 || fs[0].Rule != ranRule || fs[0].Severity != "high" || !strings.Contains(fs[0].Title, `"Simplify the discount", but discount, f now behaves differently`) ||
		!strings.Contains(fs[0].Detail, "discount(Order(100)) returned 90 before and 81 after") {
		t.Fatalf("%+v", fs)
	}
	if p := fm.prompts[0]; !strings.Contains(p, "## What running this file's methods showed") || !strings.Contains(p, "- discount(Order(100)): before 90, after 81") {
		t.Errorf("the model is shown what the run found:\n%s", p)
	}
	// The review finishes first: the run adds the finding, once, and leaves the others.
	st.ReplaceFindings(sha, []store.Finding{{SHA: sha, Path: "A.java", Line: 1, Severity: "low", Source: "model", Title: "other", Key: "k"}})
	for range 2 {
		if err := r.RanFindings(sha); err != nil {
			t.Fatal(err)
		}
	}
	if fs := st.Findings(sha); len(fs) != 2 {
		t.Errorf("added once beside the finding already there: %+v", fs)
	}
	// A commit that announces a change of behaviour is not reported for changing it.
	tr.Commit("Double the discount", map[string]string{testrepo.ServicePath: strings.Replace(testrepo.ServiceFix, "order.applyDiscount();", "order.applyDiscount(); order.applyDiscount(); // twice", 1)})
	st.SetBehaviour(tr.Head(), report)
	if err := r.Review(context.Background(), tr.Head(), Options{RulesOnly: true}); err != nil {
		t.Fatal(err)
	}
	// It gets a note beside the code, since only a person can say whether the new results are the ones meant; and a
	// difference seen only in a calling class (Other) is kept with the changed file.
	if fs := st.Findings(tr.Head()); len(fs) != 1 || fs[0].Doubt == "" ||
		!strings.Contains(fs[0].Detail, "discount(Order(100))") || !strings.Contains(fs[0].Detail, "f() returned 1 before and 2 after") {
		t.Errorf("%+v", fs)
	}
	// Whatever the message says: a call that now crashes, and a result that was never negative and now is.
	for report, want := range map[string]string{
		`{"differ":1,"differences":[{"method":"com.shop.order.OrderService#find","call":"find(1)","old":"null | args [1]","new":"throws java.lang.NullPointerException"}]}`:                                                           "now crash",
		`{"differ":1,"differences":[{"method":"com.shop.order.OrderService#find","call":"find(1)","old":"2.0 | args [1]","new":"-3.0 | args [1]"}]}`:                                                                                  "never negative",
		`{"differ":2,"differences":[{"method":"com.shop.order.OrderService#find","call":"find(1)","old":"-1 | args [1]","new":"-3 | args [1]"},{"method":"com.shop.order.OrderService#find","call":"find(2)","old":"1","new":"-2"}]}`: "",
		`{"differ":1,"differences":[{"method":"com.shop.order.OrderService#find","call":"find(1)","old":"1","new":"throws java.lang.IllegalArgumentException: no"}]}`:                                                                 "",
	} {
		st.SetBehaviour(tr.Head(), json.RawMessage(report))
		if err := r.Review(context.Background(), tr.Head(), Options{RulesOnly: true}); err != nil {
			t.Fatal(err)
		}
		fs := st.Findings(tr.Head())
		if want == "" && (len(fs) != 1 || fs[0].Doubt == "") || want != "" && (len(fs) != 1 || fs[0].Doubt != "" || !strings.Contains(fs[0].Title, want)) {
			t.Errorf("want %q for %s: %+v", want, report, fs)
		}
	}
	for msg, want := range map[string]bool{"Simplify the check": true, "Refactor: extract a helper": true, "Rename x": true, "Clean up imports": true,
		"Move the tunnel to row 8": false, "Format the score with commas": false, "Fix the simplified check": false} {
		if keepsResults.MatchString(msg) != want {
			t.Errorf("%q: keeps results = %v", msg, !want)
		}
	}
}

// A second look that confirms with a real quote but a guessing reason ("may") makes a note, unless running the code
// showed that this file's results did change: then the effect is no guess.
func TestRunEvidenceBacksAHedgedConfirmation(t *testing.T) {
	for _, ran := range []bool{false, true} {
		tr, r, st := setup(t)
		fm := &fakeModel{reply: `{"findings":[{"line":23,"severity":"high","title":"Charges twice","evidence":"payments.charge(o.getId());","detail":"d"}]}`,
			judge: `{"clears":"","proves":"payments.charge(o.getId());","reason":"The loop may charge an order twice.","verdict":"reviewer_is_right"}`}
		r.Model = fm.client(t)
		if ran {
			st.SetBehaviour(tr.Bug, json.RawMessage(`{"differ":1,"differences":[{"method":"com.shop.order.OrderService#payAll","call":"payAll([])","old":"1","new":"2"}]}`))
		}
		if err := r.Review(context.Background(), tr.Bug, Options{}); err != nil {
			t.Fatal(err)
		}
		for _, f := range st.Findings(tr.Bug) {
			if f.Source == "model" && (f.Doubt == "") != ran {
				t.Errorf("code was run and differed: %v; the model's finding: %+v", ran, f)
			}
		}
	}
}
