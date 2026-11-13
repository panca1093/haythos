package runtimecontract_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/tests/runtimecontract"
)

func fakeRuntime(t *testing.T, s runtimecontract.Script) app.Runtime {
	t.Helper()
	var steps []testfakes.RuntimeStep
	for i, st := range s.Steps {
		switch st.Kind {
		case runtimecontract.StepSay:
			for _, c := range st.Chunks {
				steps = append(steps, testfakes.Emit(app.RuntimeEvent{Kind: app.RuntimeMessage, ID: "m1", Text: c}))
			}
		case runtimecontract.StepAsk:
			steps = append(steps, testfakes.Emit(app.RuntimeEvent{Kind: app.RuntimePermissionRequest, Permission: &app.PermissionRequest{
				RequestID: "req-" + string(rune('a'+i)), Tool: "Bash", Kind: st.ReqKind, Text: st.Text, AllowOptionID: "allow", RejectOptionID: "reject",
			}}))
		case runtimecontract.StepWrite:
			steps = append(steps, testfakes.Do(func(workdir string, allowed bool) {
				if allowed {
					_ = os.WriteFile(filepath.Join(workdir, st.Path), []byte(st.Content), 0o600)
				}
			}))
		case runtimecontract.StepCrash:
			steps = append(steps, testfakes.Crash())
		case runtimecontract.StepHang:
			steps = append(steps, testfakes.Hang())
		}
	}
	return testfakes.NewRuntime("fake", testfakes.RuntimeScript{Steps: steps, CancelOnReject: s.RejectEndsTurn})
}

func TestFakeRuntimeContract(t *testing.T) {
	runtimecontract.RunRuntimeContract(t, fakeRuntime)
}
