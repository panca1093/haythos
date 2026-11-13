package contract

import "testing"

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name   string
		in     Input
		pass   bool
		reason string
	}{
		{"end turn with change", Input{Stop: EndTurn, Changed: true}, true, ""},
		{"end turn with passing checks", Input{Stop: EndTurn, Changed: true, Checks: []Check{{Name: "go-test"}, {Name: "vet"}}}, true, ""},
		{"no change", Input{Stop: EndTurn}, false, "no change in the workdir"},
		{"cancelled", Input{Stop: "cancelled", Changed: true}, false, "session stopped with cancelled"},
		{"cancelled after our reject", Input{Stop: "cancelled", Rejected: true}, false, "turn ended after a rejected request (stop reason cancelled)"},
		{"max tokens", Input{Stop: "max_tokens", Changed: true}, false, "session stopped with max_tokens"},
		{"stop beats change", Input{Stop: "refusal"}, false, "session stopped with refusal"},
		{"failing check", Input{Stop: EndTurn, Changed: true, Checks: []Check{{Name: "go-test", ExitCode: 1}}}, false, "check go-test failed (exit 1)"},
		{"first failing check is named", Input{Stop: EndTurn, Changed: true, Checks: []Check{{Name: "vet"}, {Name: "go-test", ExitCode: 2}, {Name: "lint", ExitCode: 1}}}, false, "check go-test failed (exit 2)"},
		{"check timeout", Input{Stop: EndTurn, Changed: true, Checks: []Check{{Name: "go-test", ExitCode: -1, Err: "timed out after 10m0s"}}}, false, "check go-test failed (timed out after 10m0s)"},
		{"check start error", Input{Stop: EndTurn, Changed: true, Checks: []Check{{Name: "lint", ExitCode: -1, Err: "executable not found"}}}, false, "check lint failed (executable not found)"},
		{"no change beats checks", Input{Stop: EndTurn, Checks: []Check{{Name: "go-test", ExitCode: 1}}}, false, "no change in the workdir"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(tc.in)
			if got.Pass != tc.pass || got.Reason != tc.reason {
				t.Errorf("Evaluate = {%v %q}, want {%v %q}", got.Pass, got.Reason, tc.pass, tc.reason)
			}
		})
	}
}
