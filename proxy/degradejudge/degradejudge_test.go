package degradejudge

import (
	"os"
	"strings"
	"testing"
)

// Golden values computed with pelican_judge.py's own extract() and cx().
func TestScoreMatchesPelicanJudge(t *testing.T) {
	for _, tc := range []struct {
		file                                             string
		score, bytes, shapes, grads, filters, stops, col int
		valid                                            bool
		prefix, suffix                                   string
	}{
		{"coherent.txt", 40, 1344, 14, 2, 1, 4, 12, true, "<!DOCTYPE html>\n<html><head><s", "自行车</p>\n</body></html>"},
		{"bare.txt", 10, 171, 4, 1, 0, 1, 2, true, `<html><body><svg><circle r="3"`, "</svg></body></html>"},
		{"invalid.txt", 1, 66, 1, 0, 0, 0, 0, false, "I cannot draw that, but here i", `circle r="1"/></svg>`},
	} {
		raw, err := os.ReadFile("testdata/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		html := Extract(string(raw))
		if !strings.HasPrefix(html, tc.prefix) || !strings.HasSuffix(html, tc.suffix) {
			t.Fatalf("%s: extract = %q…%q", tc.file, html[:30], html[len(html)-20:])
		}
		m := Score(html)
		if m.Score != tc.score || m.Bytes != tc.bytes || m.Shapes != tc.shapes || m.Grads != tc.grads || m.Filter != tc.filters || m.Stops != tc.stops || m.Colors != tc.col {
			t.Fatalf("%s: metrics = %+v", tc.file, m)
		}
		if Valid(html) != tc.valid {
			t.Fatalf("%s: valid = %v", tc.file, Valid(html))
		}
	}
}

func TestVerdictBoundaryAndInvalidSamples(t *testing.T) {
	if Verdict(186, DefaultThreshold) != VerdictDegraded || Verdict(187, DefaultThreshold) != VerdictOK {
		t.Fatal("186 is degraded, 187 is not")
	}
	if Verdict(186, 0) != VerdictDegraded {
		t.Fatal("0 means the default threshold")
	}
	_, _, verdict := Judge("I can't help with that.", DefaultThreshold)
	if verdict != VerdictInvalid {
		t.Fatalf("no HTML is an invalid sample, got %s", verdict)
	}
	_, m, verdict := Judge("<html><svg><circle/></svg></html>", DefaultThreshold)
	if verdict != VerdictDegraded || m.Score != 1 {
		t.Fatalf("a valid but poor sample is degraded: %s %+v", verdict, m)
	}
}
