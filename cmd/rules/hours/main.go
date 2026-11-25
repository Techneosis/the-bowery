// Command hours is a reference rule plugin (exec kind, contract 1): an
// office is open inside weekly windows in the office's timezone.
//
//	[[rules.use]]
//	name   = "work-hours"
//	plugin = "hours"
//	[rules.use.config]
//	windows = ["Mon..Fri 09:00-17:00", "Sat 10:00-13:00"]
//
// A window is days then a time range. Days: one (Mon), a range (Mon..Fri,
// wrapping past Sun), or a comma list (Mon,Wed,Fri). A range may end at
// 24:00, or before it starts (22:00-02:00), which runs into the next day.
// The verdict doesn't depend on the job, so it holds office-wide until the
// moment it would change. It reads the clock from the request, never its own.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	_ "time/tzdata" // the plugin's environment is built from nothing: carry the zones
)

const description = `{"name": "hours", "version": "1.0.0", "contracts": [1],
 "phases": ["place", "accept", "run"], "facts": [], "history": "0s",
 "config_schema": {"type": "object", "additionalProperties": false, "required": ["windows"],
  "properties": {"windows": {"type": "array", "minItems": 1, "items": {"type": "string"},
   "description": "weekly windows, e.g. Mon..Fri 09:00-17:00"}}}}`

type request struct {
	Contract int  `json:"contract"`
	Describe bool `json:"describe"`
	Rule     struct {
		Config struct {
			Windows []string `json:"windows"`
		} `json:"config"`
	} `json:"rule"`
	Clock struct {
		Now      string `json:"now"`
		Timezone string `json:"timezone"`
	} `json:"clock"`
}

type window struct {
	days       [7]bool
	start, end int // minutes from midnight; end may be <= start (runs past midnight)
}

var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func parseWindow(s string) (window, error) {
	var w window
	f := strings.Fields(s)
	if len(f) != 2 {
		return w, fmt.Errorf("window %q: want <days> <HH:MM-HH:MM>", s)
	}
	for _, part := range strings.Split(f[0], ",") {
		a, b, isRange := strings.Cut(part, "..")
		da, ok := dayNames[strings.ToLower(a)]
		if !ok {
			return w, fmt.Errorf("window %q: unknown day %q", s, a)
		}
		if !isRange {
			w.days[da] = true
			continue
		}
		db, ok := dayNames[strings.ToLower(b)]
		if !ok {
			return w, fmt.Errorf("window %q: unknown day %q", s, b)
		}
		for d := da; ; d = (d + 1) % 7 {
			w.days[d] = true
			if d == db {
				break
			}
		}
	}
	from, to, ok := strings.Cut(f[1], "-")
	if !ok {
		return w, fmt.Errorf("window %q: want HH:MM-HH:MM", s)
	}
	var err error
	if w.start, err = minutes(from, false); err != nil {
		return w, fmt.Errorf("window %q: %v", s, err)
	}
	if w.end, err = minutes(to, true); err != nil {
		return w, fmt.Errorf("window %q: %v", s, err)
	}
	if w.start == w.end {
		return w, fmt.Errorf("window %q: empty", s)
	}
	return w, nil
}

func minutes(s string, end bool) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || len(s) != 5 {
		return 0, fmt.Errorf("time %q: want HH:MM", s)
	}
	if h == 24 && m == 0 && end {
		return 24 * 60, nil
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q is not a time of day", s)
	}
	return h*60 + m, nil
}

// spans are the concrete open intervals of the windows that touch the days
// around t, in t's zone.
func spans(ws []window, t time.Time) [][2]time.Time {
	var out [][2]time.Time
	y, mo, d := t.Date()
	for off := -1; off <= 8; off++ {
		day := time.Date(y, mo, d+off, 0, 0, 0, 0, t.Location())
		for _, w := range ws {
			if !w.days[day.Weekday()] {
				continue
			}
			s := time.Date(day.Year(), day.Month(), day.Day(), w.start/60, w.start%60, 0, 0, t.Location())
			endDay := day
			if w.end <= w.start {
				endDay = day.AddDate(0, 0, 1)
			}
			e := time.Date(endDay.Year(), endDay.Month(), endDay.Day(), 0, 0, 0, 0, t.Location()).Add(time.Duration(w.end) * time.Minute)
			out = append(out, [2]time.Time{s, e})
		}
	}
	return out
}

func answer(v map[string]any) {
	b, _ := json.Marshal(v)
	os.Stdout.Write(append(b, '\n'))
}

func main() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		os.Exit(2)
	}
	var req request
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "hours: the request doesn't decode:", err)
		os.Exit(2)
	}
	if req.Contract != 1 {
		fmt.Fprintln(os.Stderr, "hours: speaks contract 1 only")
		os.Exit(2)
	}
	if req.Describe {
		var v any
		_ = json.Unmarshal([]byte(description), &v)
		b, _ := json.Marshal(v)
		os.Stdout.Write(append(b, '\n'))
		return
	}
	var ws []window
	for _, s := range req.Rule.Config.Windows {
		w, err := parseWindow(s)
		if err != nil {
			// A config error is the plugin failing, which the core turns
			// into closed with this message logged.
			fmt.Fprintln(os.Stderr, "hours:", err)
			os.Exit(2)
		}
		ws = append(ws, w)
	}
	loc, err := time.LoadLocation(req.Clock.Timezone)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hours: timezone:", err)
		os.Exit(2)
	}
	now, err := time.Parse(time.RFC3339, req.Clock.Now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hours: clock.now:", err)
		os.Exit(2)
	}
	t := now.In(loc)
	sp := spans(ws, t)
	// Open: inside a span; valid until the end of the run of overlapping spans.
	var end time.Time
	for changed := true; changed; {
		changed = false
		for _, s := range sp {
			in := !t.Before(s[0]) && t.Before(s[1])
			cont := !end.IsZero() && !s[0].After(end) && s[1].After(end)
			if (end.IsZero() && in) || cont {
				end, changed = s[1], true
			}
		}
	}
	utc := func(x time.Time) string { return x.UTC().Format(time.RFC3339) }
	if !end.IsZero() {
		answer(map[string]any{"verdict": "open", "holds": "office", "valid_until": utc(end)})
		return
	}
	var next time.Time
	for _, s := range sp {
		if s[0].After(t) && (next.IsZero() || s[0].Before(next)) {
			next = s[0]
		}
	}
	v := map[string]any{"verdict": "closed", "reason": "outside its hours (" + strings.Join(req.Rule.Config.Windows, "; ") + ")",
		"holds": "office"}
	if !next.IsZero() {
		v["retry_after"], v["valid_until"] = utc(next), utc(next)
	}
	answer(v)
}
