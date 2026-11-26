package offices

import "fmt"

// ProbeAnswer is what `office describe <office> --probe` prints for the
// resolver: the office's place-phase verdict on one job, or "deferred".
type ProbeAnswer struct {
	Office     string           `json:"office"`
	Verdict    string           `json:"verdict"`
	Reason     string           `json:"reason"`
	RetryAfter string           `json:"retry_after"`
	By         string           `json:"by"`
	Instances  []InstanceResult `json:"instances"`
}

// Deferred is the probe's answer when it can't judge for the office: the
// office judges the placement itself when it arrives (the accept phase, in
// its pull job), as it does every placement from a peer region.
const Deferred = "deferred"

// ProbeJudges says whether a probe run by caller may judge for the office.
// Judging means recording the office's decisions, and in root mode the
// recorder takes them only from the office's own user and its owners
// (RecorderCallerOK). Anyone else placing into the office, such as another
// office's user on the same machine, would be judging for an office it
// isn't, and couldn't record it if it did. Dev mode runs everything as one
// user and always judges.
func ProbeJudges(c *Config, caller string) bool {
	if c.L.Dev {
		return true
	}
	if caller == "" {
		return false
	}
	if c.Machine != nil && c.Machine.Recorder != "" && caller == c.Machine.Recorder {
		return true
	}
	return RecorderCallerOK(c, caller) == nil
}

// Probe judges job at the place phase for the resolver, as caller. A caller
// that may not judge for the office (ProbeJudges) gets Deferred, and
// nothing is evaluated, run or written: the verdict that admits a placement
// is the office's own, at the accept phase, on the signed letter, and the
// resolver plans over this office from its catalog alone (the owner's
// state and its published verdicts), as it does a peer's.
func Probe(ev *Evaluator, job *Job, caller string) (*ProbeAnswer, error) {
	c := ev.C
	if !ProbeJudges(c, caller) {
		who := caller
		if who == "" {
			who = "an unnamed caller"
		}
		return &ProbeAnswer{Office: c.Office.Name, Verdict: Deferred, Instances: []InstanceResult{},
			Reason: fmt.Sprintf("%s isn't %s's user or one of its owners, so it doesn't judge for it: %s judges it when it arrives",
				who, c.Office.Name, c.Office.Name)}, nil
	}
	res, err := ev.Evaluate("place", job)
	return &ProbeAnswer{Office: c.Office.Name, Verdict: res.Verdict, Reason: res.Reason, RetryAfter: res.RetryAfter,
		By: res.By, Instances: res.Instances}, err
}
