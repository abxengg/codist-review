package review

import "testing"

// A quote of a line the change removed stands: the review asks whether removed lines took a check away.
func TestRemovedLineQuoteStands(t *testing.T) {
	lines := []string{"public final class Collision {", "    public static boolean hits(double px, double py, double sx, double sy) {",
		"        return (int) px == (int) sx && (int) py == (int) sy;", "    }", "}"}
	patch := "@@ -1,6 +1,4 @@\n public final class Collision {\n     public static boolean hits(double px, double py, double sx, double sy) {\n" +
		"-        double dx = px - sx;\n-        double dy = py - sy;\n-        return dx * dx + dy * dy < 0.25;\n+        return (int) px == (int) sx && (int) py == (int) sy;\n     }\n }"
	if drop, _, _ := judge(lines, patch, 3, "high", "Collision by tile breaks rule 11", "return dx * dx + dy * dy < 0.25;"); drop != "" {
		t.Fatalf("a quote of the removed line was dropped: %s", drop)
	}
	if drop, _, _ := judge(lines, patch, 3, "high", "Made up", "return dx * dy;"); drop == "" {
		t.Fatal("a quote that is nowhere, not even among removed lines, must still be dropped")
	}
	// Removed, but far from the line named: dropped as before.
	far := append(lines, make([]string, 40)...)
	if drop, _, _ := judge(far, patch, 40, "high", "Collision", "return dx * dx + dy * dy < 0.25;"); drop == "" {
		t.Fatal("a removed line far from the named line must be dropped")
	}
}

// A null claim stands when the change took out a default: getOrDefault became get.
func TestRemovedDefaultBacksNullClaim(t *testing.T) {
	lines := []string{"public final class TaxRates {", "    public static int rateFor(String state) {", "        return RATES.get(state.toUpperCase());", "    }", "}"}
	patch := "@@ -1,5 +1,5 @@\n public final class TaxRates {\n     public static int rateFor(String state) {\n-        return RATES.getOrDefault(state, 1800);\n+        return RATES.get(state.toUpperCase());\n     }\n }"
	if _, _, doubt := judge(lines, patch, 3, "high", "Unknown state codes cause NullPointerException", "return RATES.get(state.toUpperCase());"); doubt != "" {
		t.Fatalf("doubted: %s", doubt)
	}
	// The clean commit from records-lab: !HR.equals(role) cannot throw, and nothing removed a default.
	lines = []string{"class Access {", "  static void requireHr(String role) {", "    if (!HR.equals(role)) {", "      throw new X();", "    }", "  }", "}"}
	patch = "@@ -1,7 +1,7 @@\n class Access {\n   static void requireHr(String role) {\n-    if (role != null && role != HR) {\n+    if (!HR.equals(role)) {\n       throw new X();\n     }\n   }\n }"
	if _, _, doubt := judge(lines, patch, 3, "high", "Null pointer exception on missing role header", "if (!HR.equals(role)) {"); doubt == "" {
		t.Fatal("the null claim on HR.equals(role) must stay doubtful")
	}
}
