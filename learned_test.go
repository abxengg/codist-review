package review

import (
	"context"
	"strings"
	"testing"
)

// A fix teaches a check when the model's pattern matches the mistake, not the fix, and little else; the check then
// catches the same mistake in a later commit with no model.
func TestLearnedChecks(t *testing.T) {
	tr, r, st := setup(t)
	const p = "order-service/src/main/java/com/shop/order/Pages.java"
	page := func(body string) string {
		return "package com.shop.order;\n\nclass Pages {\n    int count(int items, int size) {\n" + body + "    }\n}\n"
	}
	tr.Commit("Count pages", map[string]string{p: page("        int pages = items / size;\n        return pages;\n")})
	tr.Commit("Fix the last page being dropped", map[string]string{p: page("        int pages = (items + size - 1) / size;\n        return pages;\n")})
	fix := tr.Head()
	fm := &fakeModel{reply: `{"findings":[]}`, learn: `{"pattern":"int \\w+ = \\w+ / \\w+;","title":"Division drops the last partial page","detail":"Integer division rounds down."}`}
	r.Model = fm.client(t)
	if err := r.Review(context.Background(), fix, Options{}); err != nil {
		t.Fatal(err)
	}
	learned := st.Learned()
	if len(learned) != 1 || learned[0].FixSHA != fix {
		t.Fatalf("learned %+v", learned)
	}
	if rv, _ := st.GetReview(fix); !strings.Contains(rv.Note, "learned 1 check") {
		t.Errorf("note %q", rv.Note)
	}
	// The same mistake elsewhere, later: found by the check alone.
	const q = "order-service/src/main/java/com/shop/order/Rows.java"
	tr.Commit("Count rows", map[string]string{q: "package com.shop.order;\n\nclass Rows {\n    int rows(int cells, int perRow) {\n        int rows = cells / perRow;\n        return rows;\n    }\n}\n"})
	r.Model = nil
	if err := r.Review(context.Background(), tr.Head(), Options{RulesOnly: true}); err != nil {
		t.Fatal(err)
	}
	fs := st.Findings(tr.Head())
	if len(fs) != 1 || !strings.HasPrefix(fs[0].Rule, "learned-") || fs[0].Line != 5 || !strings.Contains(fs[0].Detail, "learned this check from commit "+fix[:7]) {
		t.Errorf("%+v", fs)
	}
	// Patterns that do not prove themselves are not kept.
	c, _ := r.Repo.Get(fix)
	changes, _ := r.Repo.Changes(fix)
	before, after := patchSides(changes[0].Patch)
	for pattern, why := range map[string]string{"": "no mistake", "(": "not a valid", "return": "does not match the mistake", "int pages": "matches the fix too", "\\w+": "matches the fix too"} {
		if _, got := r.prove(c, changes[0], before, after, pattern, "t", "d"); !strings.Contains(got, why) {
			t.Errorf("%q: %q", pattern, got)
		}
	}
	if _, got := r.prove(c, changes[0], before, after, "int pages = items / \\w+;", "t", "d"); got != "" {
		t.Errorf("a proper pattern was refused: %s", got)
	}
}
