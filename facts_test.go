package review

import (
	"context"
	"strings"
	"testing"

	"github.com/abxengg/codist-git/testrepo"
)

// A method that other files call and that can now return null is reported with its callers; one whose promise did
// not change is not, and a method no other file calls is not asked about. Only the deep pass asks.
func TestPromiseChanges(t *testing.T) {
	tr, r, st := setup(t)
	strict := strings.Replace(testrepo.ServiceFix, "orders.findById(id).orElse(null);", "orders.findById(id).orElseThrow();", 1)
	tr.Commit("Fail on a missing order", map[string]string{testrepo.ServicePath: strict})
	tr.Commit("Return nothing for a missing order", map[string]string{testrepo.ServicePath: testrepo.ServiceFix})
	fm := &fakeModel{reply: `{"findings":[]}`, judge: `{"verdict":"cannot_tell","reason":"r","proves":"","clears":""}`}
	r.Model = fm.client(t)
	// An ordinary review does not ask: the questions cost model time, so they belong to the deep pass.
	if err := r.Review(context.Background(), tr.Head(), Options{}); err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Findings(tr.Head()) {
		if strings.Contains(f.Title, "can now") {
			t.Fatalf("an ordinary review asked about promises: %s", f.Title)
		}
	}
	if err := r.Review(context.Background(), tr.Head(), Options{Deep: true}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range st.Findings(tr.Head()) {
		got = append(got, f.Title+" | "+f.Detail)
	}
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "find can now return null; callers in other files rely on the old behaviour") ||
		!strings.Contains(all, "OrderController.get (OrderController.java:") || !strings.Contains(all, "return service.find(id);") {
		t.Fatalf("%s", all)
	}
	asked := 0
	for _, p := range fm.prompts {
		if strings.HasPrefix(p, "You are shown one Java method") {
			asked++
		}
	}
	if asked != 2 {
		t.Errorf("%d promise questions; want before and after of find only", asked)
	}
	// The other way round, the promise only got stronger: nothing to report.
	tr.Commit("Fail again on a missing order", map[string]string{testrepo.ServicePath: strict})
	if err := r.Review(context.Background(), tr.Head(), Options{Deep: true}); err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Findings(tr.Head()) {
		if strings.Contains(f.Title, "can now") {
			t.Errorf("a stronger promise was reported: %s", f.Title)
		}
	}
}
