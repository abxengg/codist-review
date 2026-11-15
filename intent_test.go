package review

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abxengg/codist-review/store"
)

func TestIntent(t *testing.T) {
	tr, r, _ := setup(t)
	report := json.RawMessage(`{"sha":"x","status":"done","differ":2,"differences":[{"method":"a.B#c","call":"c(0)","old":"1","new":"2"},{"method":"a.B#d","call":"d(null)","old":"3","new":"throws"}]}`)
	if got := r.Intent(context.Background(), tr.Fix, report); string(got) != string(report) {
		t.Error("without a model the report stays as it was")
	}
	fm := &fakeModel{reply: `{"about":"changes-results","aboutWhy":"It says rethrow.","differences":[{"index":2,"reason":"The message speaks of rethrowing.","intent":"intended"},{"index":1,"reason":"Nothing covers it.","intent":"unintended"},{"index":9,"reason":"x","intent":"intended"}]}`}
	r.Model = fm.client(t)
	var rep struct {
		Unintended  int
		Differences []struct{ Call, Intent, Why string }
	}
	if err := json.Unmarshal(r.Intent(context.Background(), tr.Fix, report), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Unintended != 1 || len(rep.Differences) != 2 || rep.Differences[0].Intent != "unintended" || rep.Differences[1].Intent != "intended" ||
		rep.Differences[1].Why != "The message speaks of rethrowing." {
		t.Errorf("%+v", rep)
	}
	for _, want := range []string{"## Commit message\nRethrow discount errors", "throw new IllegalStateException(e);", "1. c(0)", "after:  throws"} {
		if !strings.Contains(fm.prompts[0], want) {
			t.Errorf("prompt lacks %q:\n%s", want, fm.prompts[0])
		}
	}
	r.Intent(context.Background(), tr.Fix, report)
	if len(fm.prompts) != 1 { // asked once so far
		t.Error("the same question is answered from the cache")
	}
	// A commit that says it only tidies meant none of the differences, whatever the model says call by call.
	fm.reply = `{"about":"keeps-results","aboutWhy":"The message says simplify.","differences":[{"index":1,"reason":"x","intent":"intended"}]}`
	tidy := json.RawMessage(strings.Replace(string(report), "c(0)", "c(5)", 1))
	if err := json.Unmarshal(r.Intent(context.Background(), tr.Fix, tidy), &rep); err != nil || rep.Unintended != 2 || rep.Differences[0].Why != "The message says simplify." {
		t.Errorf("%v %+v", err, rep)
	}
	fm.status = 500
	other := json.RawMessage(strings.Replace(string(report), "c(0)", "c(7)", 1))
	if got := r.Intent(context.Background(), tr.Fix, other); string(got) != string(other) {
		t.Error("a failing model leaves the report as it was")
	}
}

func TestTrainingSet(t *testing.T) {
	tr, r, st := setup(t)
	f := func(key, source string, line int) store.Finding {
		return store.Finding{SHA: tr.Bug, Path: "order-service/src/main/java/com/shop/order/OrderService.java", Line: line, EndLine: line,
			Severity: "high", Source: source, Title: "Title " + key, Detail: "Detail", Key: key}
	}
	st.SetReview(store.Review{SHA: tr.Bug, Status: "done"})
	if err := st.ReplaceFindings(tr.Bug, []store.Finding{f("fixed", "model", 30), f("wrong", "model", 23), f("useful", "model", 17), f("open", "model", 18), f("rule", "rule", 30)}); err != nil {
		t.Fatal(err)
	}
	st.Resolve([]string{"fixed", "rule"}, tr.Fix)
	st.SetFeedback("wrong", "wrong", "")
	st.SetFeedback("useful", "useful", "")
	var out bytes.Buffer
	useful, wrong, err := r.TrainingSet(&out)
	if err != nil || useful != 2 || wrong != 1 {
		t.Fatalf("%d useful, %d wrong, %v", useful, wrong, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var ex struct {
		Messages []struct{ Role, Content string }
	}
	if len(lines) != 3 || json.Unmarshal([]byte(lines[0]), &ex) != nil || len(ex.Messages) != 3 || ex.Messages[2].Role != "assistant" {
		t.Fatalf("%s", out.String())
	}
	if !strings.Contains(out.String(), "Title wrong") || strings.Contains(out.String(), "Title open") || strings.Contains(out.String(), "Title rule") ||
		!strings.Contains(out.String(), "} catch (Exception e) {") {
		t.Errorf("only the model's decided findings, with their code:\n%s", out.String())
	}
}
