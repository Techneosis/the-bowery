package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Techneosis/the-bowery/internal/offices"
	"github.com/Techneosis/the-bowery/pkg/bus"
)

// ---------------------------------------------------------------- bus

func cmdBus(args []string) {
	if len(args) == 0 {
		die("bus: serve, publish, follow or status (office --help)")
	}
	switch args[0] {
	case "serve":
		busServe(args[1:])
	case "publish":
		busPublish(args[1:])
	case "follow":
		busFollow(args[1:])
	case "status":
		busStatus(args[1:])
	default:
		die("bus: unknown command %s: serve, publish, follow or status", args[0])
	}
}

// busServe runs the region's local bus node on this machine: a unix socket
// under the var directory, whose peer (the Unix user the kernel saw
// connect) is the seat, and the log beside it.
func busServe(args []string) {
	if f := parse(args, nil, nil); f.office != "" {
		die("bus serve takes no arguments")
	}
	reg, err := offices.LoadRegion(layout)
	if err != nil {
		die("%v", err)
	}
	m, err := offices.LoadMachine(layout)
	if err != nil {
		die("%v", err)
	}
	if here := offices.ThisMachine(); m.Machine != here {
		die("%s says machine %q, and this is %q", layout.MachineFile(), m.Machine, here)
	}
	if !layout.Dev && os.Geteuid() == 0 {
		die("bus serve doesn't run as root: give it its own user, which can read the region key (the install doc)")
	}
	if m.RegionKey == "" {
		die("%s has no region_key: the bus node seals its seats' events with the region's private key", layout.MachineFile())
	}
	key, err := offices.LoadPrivateKey(expandHome(m.RegionKey))
	if err != nil {
		die("the region key: %v", err)
	}
	if pin, err := offices.ParsePublicKey(reg.Key); err != nil || !pin.Equal(key.Public) {
		die("region_key (%s) isn't region %s's key, as its region file says it", key.Public.Fingerprint(), reg.Region)
	}
	if err := os.MkdirAll(layout.BusDir(), 0o755); err != nil {
		die("%v", err)
	}
	// The node signs its seats' events and seals them with the region key,
	// under two namespaces: a seat has no key of its own on this machine,
	// and the node is what vouches that the seat said it.
	// Each peer's pinned region key: it signs the peer's calls, seals its
	// events and signs for its seats, as this region's does here.
	peerKeys := bus.PeerKeys{}
	for name, p := range reg.Peers {
		if pk, err := offices.ParsePublicKey(p.Key); err == nil {
			peerKeys[name] = pk
		}
	}
	n, err := bus.NewNode(bus.NodeConfig{
		Dir: layout.BusLog(), Region: reg.Region, Rules: bus.RulesOf(reg),
		Sign: key, Seal: key, Peers: peerKeys, Dev: layout.Dev, Now: now,
		Logf: func(format string, a ...any) { fmt.Fprintf(os.Stderr, "office bus: "+format+"\n", a...) },
	})
	if err != nil {
		die("%v", err)
	}
	defer n.Close()

	// The node holds the log's lock, so a socket left at the path is a
	// dead node's.
	sock := layout.BusSocket()
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		die("%v", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		die("%v", err)
	}
	ln.SetUnlinkOnClose(true)
	// Any local user may connect: the region's rules decide what its seat
	// may do, and a user with no seat may do nothing.
	perm := os.FileMode(0o666)
	if layout.Dev {
		perm = 0o600
	}
	if err := os.Chmod(sock, perm); err != nil {
		die("%v", err)
	}
	fmt.Printf("listening on %s\n", sock)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "office bus: "+format+"\n", a...) }
	// Writers the node didn't see (another handle on the log) wake its
	// followers through the log directory's watch; its backstop covers a
	// platform with none.
	if w, err := offices.WatchAppends(layout.BusLog()); err == nil {
		defer w.Close()
		go func() {
			for ctx.Err() == nil {
				if ok, err := w.Wait(ctx, time.Now().Add(time.Minute)); err != nil {
					return
				} else if ok {
					n.Wake()
				}
			}
		}()
	}
	// Replicate with every peer this machine can reach (one with an
	// address) and has something to exchange with. A peer with no address
	// reaches this node instead: it makes both calls, and this node
	// answers them.
	for _, name := range sortedPeers(reg) {
		p := reg.Peers[name]
		push, pull := len(p.MayRead) > 0, len(p.MayPublish) > 0
		if p.Address == "" || peerKeys[name].Key == nil || (!push && !pull) {
			continue
		}
		var link bus.Link
		switch m.Transport {
		case "http":
			link = bus.HTTPLink{URL: p.Address}
		case "", "dir":
			link = bus.SocketLink{Path: filepath.Join(p.Address, "bus", "node.sock")}
		default:
			logf("peer %s: transport %q has no bus link in this build (dir, http)", name, m.Transport)
			continue
		}
		go n.Replicate(ctx, bus.Peering{
			Peer: name, Link: link, StateFile: filepath.Join(layout.BusDir(), "peers", name+".json"),
			Push: push, Pull: pull, Logf: logf,
		})
	}
	go func() {
		for {
			if d, err := n.Compact(); err != nil {
				logf("retention: %v", err)
			} else if d > 0 {
				logf("retention dropped %d event(s)", d)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logf("%v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func() {
			seat, err := peerUser(c)
			if err != nil {
				if !layout.Dev {
					b, _ := json.Marshal(bus.Reply{Error: "who's asking? " + err.Error()})
					c.Write(append(b, '\n'))
					c.Close()
					return
				}
				seat = offices.Whoami()
			}
			n.Serve(c, seat)
		}()
	}
}

func sortedPeers(reg *offices.Region) []string {
	out := make([]string, 0, len(reg.Peers))
	for name := range reg.Peers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// busRequest sends one request to the node and returns its replies.
func busRequest(req bus.Request) *bufio.Reader {
	if as := os.Getenv("OFFICE_BUS_AS"); as != "" {
		if !layout.Dev {
			die("OFFICE_BUS_AS is for dev mode only: the seat is your Unix user")
		}
		req.As = as
	}
	sock := layout.BusSocket()
	c, err := net.Dial("unix", sock)
	if err != nil {
		die("no bus node at %s (office bus serve): %v", sock, err)
	}
	b, err := json.Marshal(req)
	if err != nil {
		die("%v", err)
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		die("the bus node: %v", err)
	}
	return bufio.NewReaderSize(c, 128<<10)
}

func busReply(rd *bufio.Reader) (bus.Reply, bool) {
	line, err := rd.ReadBytes('\n')
	if err != nil {
		return bus.Reply{}, false
	}
	var r bus.Reply
	if err := json.Unmarshal(line, &r); err != nil {
		die("the bus node said something that isn't a reply: %v", err)
	}
	return r, true
}

func jsonFlag(f flags, name string) json.RawMessage {
	v, ok := f.vals[name]
	if !ok {
		return nil
	}
	if !json.Valid([]byte(v)) {
		die("%s isn't JSON: %s", name, v)
	}
	return json.RawMessage(v)
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func busPublish(args []string) {
	f := parse(args, []string{"--owner", "--subject", "--actor", "--data", "--cause"}, []string{"--json"})
	if f.office == "" {
		die("bus publish: which topic? (office bus publish job.started)")
	}
	p := bus.Publication{
		Kind: f.office, Owners: splitList(f.vals["--owner"]),
		Subject: jsonFlag(f, "--subject"), Actor: jsonFlag(f, "--actor"), Data: jsonFlag(f, "--data"),
	}
	if v, ok := f.vals["--cause"]; ok {
		i := strings.LastIndexByte(v, ':')
		var seq uint64
		var err error
		if i > 0 {
			seq, err = strconv.ParseUint(v[i+1:], 10, 64)
		}
		if i <= 0 || err != nil || seq == 0 || !bus.ValidProducer(v[:i]) {
			die("--cause takes producer:seq (region-a/desk:12), not %q", v)
		}
		p.Cause = &bus.Ref{Producer: v[:i], Seq: seq}
	}
	r, ok := busReply(busRequest(bus.Request{Op: "publish", Publication: &p}))
	switch {
	case !ok:
		die("the bus node closed the connection without an answer")
	case r.Error != "":
		die("%s", r.Error)
	case r.Ref == nil || r.Record == nil:
		die("the bus node's answer names no record")
	case f.bools["--json"]:
		fmt.Println(string(*r.Record))
	default:
		fmt.Println(r.Ref.String())
	}
}

// busFollow prints each record the seat may read past --cursor, one line of
// JSON each, as they come; --once stops when it's caught up. Exit 3 is the
// node letting go of a reader that fell behind: resume from the cursor it
// names.
func busFollow(args []string) {
	f := parse(args, []string{"--cursor", "--topic"}, []string{"--once"})
	if f.office != "" {
		die("bus follow takes no topic: --topic job.*,card.answered")
	}
	rd := busRequest(bus.Request{Op: "follow", Cursor: f.vals["--cursor"], Topics: splitList(f.vals["--topic"]),
		Once: f.bools["--once"]})
	out := bufio.NewWriter(os.Stdout)
	caught := false
	for {
		r, ok := busReply(rd)
		if !ok {
			out.Flush()
			if caught && f.bools["--once"] {
				return
			}
			die("the bus node closed the connection")
		}
		switch {
		case r.Error != "":
			out.Flush()
			if r.Floors != "" {
				die("%s: resync, then follow again from --cursor %s", r.Error, r.Floors)
			}
			die("%s", r.Error)
		case r.Record != nil:
			out.Write(*r.Record)
			out.WriteByte('\n')
			if rd.Buffered() == 0 {
				out.Flush()
			}
		case r.CaughtUp:
			caught = true
			out.Flush()
			fmt.Fprintf(os.Stderr, "office bus: caught up at %q\n", r.Cursor)
		case r.End != "":
			out.Flush()
			fmt.Fprintf(os.Stderr, "office bus: %s\n", r.End)
			os.Exit(3)
		}
	}
}

func busStatus(args []string) {
	f := parse(args, nil, []string{"--json"})
	if f.office != "" {
		die("bus status takes no arguments")
	}
	r, ok := busReply(busRequest(bus.Request{Op: "status"}))
	switch {
	case !ok:
		die("the bus node closed the connection without an answer")
	case r.Error != "":
		die("%s", r.Error)
	case r.Status == nil:
		die("the bus node's answer has no status")
	case f.bools["--json"]:
		printJSON(r.Status)
		return
	}
	s := r.Status
	dash := func(v string) string {
		if v == "" {
			return "-"
		}
		return v
	}
	for _, l := range [][2]string{
		{"region", s.Region}, {"seat", s.Seat}, {"producer", s.Producer},
		{"heads", dash(s.Heads)}, {"floors", dash(s.Floors)},
		{"publishes", dash(strings.Join(s.MayPublish, ", "))}, {"reads", dash(strings.Join(s.MayRead, ", "))},
		{"followers", strconv.FormatInt(s.Followers, 10)}, {"socket", layout.BusSocket()},
	} {
		fmt.Printf("%-10s%s\n", l[0], l[1])
	}
	for _, p := range s.Peers {
		line := fmt.Sprintf("%s: pushed %s, pulled %s", p.Peer, dash(p.Pushed), dash(p.Pulled))
		if p.Error != "" {
			line += " (last error " + p.ErrorAt.Format(time.RFC3339) + ": " + p.Error + ")"
		}
		if p.Gap != "" {
			line += " (retention dropped before it was had: " + p.Gap + ")"
		}
		fmt.Printf("%-10s%s\n", "peer", line)
	}
}
