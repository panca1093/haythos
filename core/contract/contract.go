package contract

import "fmt"

const EndTurn = "end_turn"

type Check struct {
	Name     string
	ExitCode int
	// Err is set when the check did not run to completion (start error, timeout).
	Err string
}

type Input struct {
	Stop string
	// Rejected is true when the kernel rejected a request in this session; some runtimes then end the turn as cancelled.
	Rejected bool
	Changed  bool
	Checks   []Check
}

type Verdict struct {
	Pass   bool
	Reason string
}

// Evaluate judges a delegated session: end_turn, a change in the workdir, then every check in order.
func Evaluate(in Input) Verdict {
	switch {
	case in.Stop == "cancelled" && in.Rejected:
		return Verdict{Reason: "turn ended after a rejected request (stop reason cancelled)"}
	case in.Stop != EndTurn:
		return Verdict{Reason: fmt.Sprintf("session stopped with %s", in.Stop)}
	case !in.Changed:
		return Verdict{Reason: "no change in the workdir"}
	}
	for _, c := range in.Checks {
		switch {
		case c.Err != "":
			return Verdict{Reason: fmt.Sprintf("check %s failed (%s)", c.Name, c.Err)}
		case c.ExitCode != 0:
			return Verdict{Reason: fmt.Sprintf("check %s failed (exit %d)", c.Name, c.ExitCode)}
		}
	}
	return Verdict{Pass: true}
}
