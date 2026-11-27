package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/Techneosis/the-bowery/pkg/bus"
)

// ---------------------------------------------------------------- private

func cmdPrivate(args []string) {
	if len(args) == 0 {
		die("private: ls or show (office --help)")
	}
	switch args[0] {
	case "ls":
		privateLs(args[1:])
	case "show":
		privateShow(args[1:])
	default:
		die("private: unknown command %s: ls or show", args[0])
	}
}

// privateFold reads every private.* record your seat may read from the
// local node, to the heads, and folds them.
func privateFold() bus.PrivateState {
	rd := busRequest(bus.Request{Op: "follow", Topics: []string{"private.*"}, Once: true})
	f := bus.NewPrivateFold()
	for {
		r, ok := busReply(rd)
		if !ok {
			die("the bus node closed the connection before it was caught up")
		}
		switch {
		case r.Error != "":
			die("%s", r.Error)
		case r.Record != nil:
			rec, err := bus.ParseLine(*r.Record)
			if err != nil {
				die("the bus node sent a record that doesn't read: %v", err)
			}
			f.Apply(rec)
		case r.CaughtUp:
			return f.State()
		case r.End != "":
			die("%s", r.End)
		}
	}
}

func privateLs(args []string) {
	f := parse(args, []string{"--type"}, []string{"--json"})
	if f.office != "" {
		die("private ls takes no arguments: --type card|request|ticket|hide|note")
	}
	only := f.vals["--type"]
	if only != "" && !contains(bus.PrivateLists, only) {
		die("--type %q: %s", only, strings.Join(bus.PrivateLists, ", "))
	}
	s := privateFold()
	if f.bools["--json"] {
		if only != "" {
			for _, l := range bus.PrivateLists {
				if l != only {
					s.Lists[l] = []bus.PrivateEntry{}
				}
			}
		}
		b, err := s.Bytes()
		if err != nil {
			die("%v", err)
		}
		fmt.Println(string(b))
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	n := 0
	for _, l := range bus.PrivateLists {
		if only != "" && l != only {
			continue
		}
		for _, e := range s.Lists[l] {
			var v map[string]any
			_ = json.Unmarshal(e.V, &v)
			str := func(k string) string { s, _ := v[k].(string); return s }
			state, what := str("state"), ""
			switch l {
			case "card":
				what = str("subject")
			case "request", "ticket":
				what = str("t")
			case "hide":
				state, what = "hidden", "from your view"
			case "note":
				state, what = "on "+str("on"), str("text")
			}
			if len(what) > 72 {
				what = what[:69] + "..."
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", l, e.Key, state, what)
			n++
		}
	}
	tw.Flush()
	if n == 0 {
		fmt.Fprintln(os.Stderr, "office: no private records your seat reads")
	}
	var skipped []bus.PrivateSkip
	_ = json.Unmarshal(s.Meta["skipped"], &skipped)
	for _, sk := range skipped {
		fmt.Fprintf(os.Stderr, "office: skipped %s (%s): %s\n", sk.Ref, sk.Kind, sk.Error)
	}
}

func privateShow(args []string) {
	f := parse(args, nil, []string{"--json"})
	if f.office == "" {
		die("private show: which key? (office private show watching)")
	}
	key := f.office
	s := privateFold()
	l, e, ok := s.Find(key)
	if !ok && !strings.HasPrefix(key, bus.PrivatePrefix) {
		l, e, ok = s.Find(bus.PrivatePrefix + key)
	}
	if !ok {
		die("no private record %s your seat reads (office private ls)", key)
	}
	out := struct {
		Type string          `json:"type"`
		Key  string          `json:"key"`
		V    json.RawMessage `json:"v"`
	}{l, e.Key, e.V}
	if f.bools["--json"] {
		printJSON(out)
		return
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		die("%v", err)
	}
	fmt.Println(string(b))
}
