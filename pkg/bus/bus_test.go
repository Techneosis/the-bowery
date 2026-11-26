package bus

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Techneosis/the-bowery/internal/offices"
)

func key(t *testing.T) offices.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return offices.NewPrivateKey(priv)
}

// keys is a fixed set of pinned keys, the way a node's config pins them.
type keys struct {
	producers map[string]offices.PublicKey
	regions   map[string]offices.PublicKey
}

func (k keys) ProducerKeys(p string) []offices.PublicKey {
	if pk, ok := k.producers[p]; ok {
		return []offices.PublicKey{pk}
	}
	return nil
}

func (k keys) RegionKeys(r string) []offices.PublicKey {
	if pk, ok := k.regions[r]; ok {
		return []offices.PublicKey{pk}
	}
	return nil
}

type world struct {
	seat, region offices.PrivateKey
	keys         keys
}

func newWorld(t *testing.T) world {
	w := world{seat: key(t), region: key(t)}
	w.keys = keys{
		producers: map[string]offices.PublicKey{"north/desk": w.seat.Public, "records": w.seat.Public},
		regions:   map[string]offices.PublicKey{"north": w.region.Public},
	}
	return w
}

func ev(seq uint64) Event {
	return Event{
		V:        1,
		Producer: "north/desk",
		Seq:      seq,
		Region:   "north",
		At:       time.Date(2030, 1, 2, 3, 4, 5, 600_000_000, time.UTC),
		Kind:     "job.started",
		Owners:   []string{"alice"},
		Subject:  json.RawMessage(`{"work":"key:demo","job":"j-1"}`),
		Data:     json.RawMessage(`{"b":2,"a":"x<y"}`),
	}
}

func (w world) sign(t *testing.T, e Event) Record {
	t.Helper()
	r, err := Sign(e, w.seat, w.region)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The canonical bytes don't depend on how the event's JSON was spelled:
// key order and spacing in its free-form parts don't change them.
func TestCanonicalIgnoresSpelling(t *testing.T) {
	a := ev(1)
	b := ev(1)
	b.Data = json.RawMessage(" { \"a\" : \"x<y\", \"b\": 2 } ")
	b.Subject = json.RawMessage(`{"job":"j-1",  "work":"key:demo"}`)
	ca, err := a.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	cb, err := b.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca, cb) {
		t.Fatalf("canonical bytes differ:\n%s\n%s", ca, cb)
	}
	if !strings.Contains(string(ca), `"a":"x<y"`) {
		t.Fatalf("canonical bytes escape HTML or reorder: %s", ca)
	}
	if !strings.Contains(string(ca), `"at":"2030-01-02T03:04:05.6Z"`) {
		t.Fatalf("time isn't UTC RFC 3339: %s", ca)
	}
}

// A record verifies against the pinned keys, survives a round trip through
// its wire line, and any change to the event breaks it.
func TestSignVerify(t *testing.T) {
	w := newWorld(t)
	r := w.sign(t, ev(1))
	line, err := r.Line()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(line, []byte("\n")) != 1 || line[len(line)-1] != '\n' {
		t.Fatalf("a record's line is one line: %q", line)
	}
	got, err := ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Verify(w.keys); err != nil {
		t.Fatalf("a good record doesn't verify: %v", err)
	}
	if got.Event.Kind != "job.started" || got.Event.Seq != 1 {
		t.Fatalf("round trip lost the event: %+v", got.Event)
	}

	tampered := got
	tampered.Event.Owners = []string{"mallory"}
	if err := tampered.Verify(w.keys); err == nil {
		t.Fatal("a changed audience still verifies")
	}
}

// The producer's signature and the region's seal are separate, and each
// must be by its own pinned key, in its own namespace.
func TestVerifyRefuses(t *testing.T) {
	w := newWorld(t)
	other := key(t)

	cases := map[string]func() Record{
		"unknown producer": func() Record {
			e := ev(1)
			e.Producer = "north/stranger"
			return w.sign(t, e)
		},
		"signed by the wrong key": func() Record {
			r, err := Sign(ev(1), other, w.region)
			if err != nil {
				t.Fatal(err)
			}
			return r
		},
		"sealed by the wrong region key": func() Record {
			r, err := Sign(ev(1), w.seat, other)
			if err != nil {
				t.Fatal(err)
			}
			return r
		},
		"seal swapped for the signature": func() Record {
			r := w.sign(t, ev(1))
			// The seat's signature, offered as the region's seal: same
			// bytes, wrong namespace and wrong key.
			r.Seal = r.Sig
			return r
		},
		"a region's producer sealed by another region": func() Record {
			e := ev(1)
			e.Region = "south"
			ks := w.keys
			ks.regions = map[string]offices.PublicKey{"north": w.region.Public, "south": w.region.Public}
			r := w.sign(t, e)
			if err := r.Verify(ks); err == nil {
				t.Fatal("north/desk sealed as south verifies")
			}
			return r
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			if err := mk().Verify(w.keys); err == nil {
				t.Fatal("verified")
			}
		})
	}

	// A bare producer (a service, not a region's seat) may be sealed by the
	// region it lives in.
	e := ev(1)
	e.Producer = "records"
	if err := w.sign(t, e).Verify(w.keys); err != nil {
		t.Fatalf("a bare producer sealed by its region: %v", err)
	}
}

func TestValidate(t *testing.T) {
	bad := map[string]func(*Event){
		"version":        func(e *Event) { e.V = 2 },
		"no producer":    func(e *Event) { e.Producer = "" },
		"producer chars": func(e *Event) { e.Producer = "north/Desk Two" },
		"producer depth": func(e *Event) { e.Producer = "north/desk/x" },
		"seq zero":       func(e *Event) { e.Seq = 0 },
		"no region":      func(e *Event) { e.Region = "" },
		"no time":        func(e *Event) { e.At = time.Time{} },
		"kind one part":  func(e *Event) { e.Kind = "job" },
		"kind chars":     func(e *Event) { e.Kind = "job.Started" },
		"kind wildcard":  func(e *Event) { e.Kind = "job.*" },
		"owner chars":    func(e *Event) { e.Owners = []string{"al ice"} },
		"data not json":  func(e *Event) { e.Data = json.RawMessage(`{`) },
		"data too big": func(e *Event) {
			e.Data = json.RawMessage(`"` + strings.Repeat("x", MaxEventBytes) + `"`)
		},
		"cause no seq": func(e *Event) { e.Cause = &Ref{Producer: "records"} },
	}
	for name, f := range bad {
		t.Run(name, func(t *testing.T) {
			e := ev(1)
			f(&e)
			if err := e.Validate(); err == nil {
				t.Fatal("valid")
			}
		})
	}
	e := ev(1)
	e.Cause = &Ref{Producer: "records", Seq: 48213}
	if err := e.Validate(); err != nil {
		t.Fatalf("a good event: %v", err)
	}
}

// A strict decoder: a field nobody knows is refused, not dropped.
func TestParseLineStrict(t *testing.T) {
	w := newWorld(t)
	line, _ := w.sign(t, ev(1)).Line()
	extra := bytes.Replace(line, []byte(`{"event":`), []byte(`{"extra":1,"event":`), 1)
	if _, err := ParseLine(extra); err == nil {
		t.Fatal("an unknown field parsed")
	}
	if _, err := ParseLine(append(bytes.TrimSuffix(line, []byte("\n")), line...)); err == nil {
		t.Fatal("two records on one line parsed")
	}
}

func TestCursor(t *testing.T) {
	c, err := ParseCursor("records=48213,north/desk=912")
	if err != nil {
		t.Fatal(err)
	}
	if c["records"] != 48213 || c["north/desk"] != 912 {
		t.Fatalf("parsed %v", c)
	}
	if got := c.String(); got != "north/desk=912,records=48213" {
		t.Fatalf("a cursor's string is sorted: %q", got)
	}
	empty, err := ParseCursor("")
	if err != nil || len(empty) != 0 || empty.String() != "" {
		t.Fatalf("the empty cursor: %v %v", empty, err)
	}
	for _, s := range []string{"records", "records=x", "records=1,records=2", "Rec=1", "records=0", "a=1,,b=2"} {
		if _, err := ParseCursor(s); err == nil {
			t.Errorf("%q parsed", s)
		}
	}
	c.Advance(Ref{Producer: "records", Seq: 48000})
	if c["records"] != 48213 {
		t.Fatal("a cursor went backwards")
	}
	c.Advance(Ref{Producer: "south/x", Seq: 3})
	if !c.Covers(Ref{Producer: "south/x", Seq: 2}) || c.Covers(Ref{Producer: "south/x", Seq: 4}) {
		t.Fatalf("covers: %v", c)
	}
}

func openLog(t *testing.T, dir string, w world) *Log {
	t.Helper()
	l, err := OpenLog(dir, w.keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// The log keeps each producer's records in seq order, drops an exact
// duplicate, refuses a different record at a seq it already has (a fork)
// and anything older than its last, and allows gaps (a producer may number
// its events by a table's id, where not every row goes out).
func TestLogAppend(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	l := openLog(t, dir, w)

	for _, seq := range []uint64{1, 2, 5} {
		added, err := l.Append(w.sign(t, ev(seq)))
		if err != nil || !added {
			t.Fatalf("append %d: %v %v", seq, added, err)
		}
	}
	added, err := l.Append(w.sign(t, ev(2)))
	if err != nil || added {
		t.Fatalf("an exact duplicate: added=%v err=%v", added, err)
	}
	fork := ev(5)
	fork.Kind = "job.done"
	if _, err := l.Append(w.sign(t, fork)); !errors.Is(err, ErrFork) {
		t.Fatalf("a fork at 5: %v", err)
	}
	if _, err := l.Append(w.sign(t, ev(4))); !errors.Is(err, ErrBehind) {
		t.Fatalf("a record behind the head: %v", err)
	}
	bad := w.sign(t, ev(6))
	bad.Event.Owners = []string{"mallory"}
	if _, err := l.Append(bad); err == nil {
		t.Fatal("a record that doesn't verify was appended")
	}
	if got := l.Heads(); got.String() != "north/desk=5" {
		t.Fatalf("heads %v", got)
	}

	// The log is on disk: a reopened one has the same records and heads.
	l.Close()
	l2 := openLog(t, dir, w)
	if got := l2.Heads(); got.String() != "north/desk=5" {
		t.Fatalf("heads after reopen %v", got)
	}
	recs, err := l2.Since(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for _, r := range recs {
		seqs = append(seqs, r.Event.Seq)
	}
	if len(seqs) != 3 || seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 5 {
		t.Fatalf("records after reopen: %v", seqs)
	}
	if _, err := l2.Append(w.sign(t, fork)); !errors.Is(err, ErrFork) {
		t.Fatalf("a fork after reopen: %v", err)
	}
}

// Since reads past a vector cursor: each producer from just after its own
// position, a producer the cursor doesn't name from its start, with a limit.
func TestLogSince(t *testing.T) {
	w := newWorld(t)
	l := openLog(t, t.TempDir(), w)
	for seq := uint64(1); seq <= 4; seq++ {
		if _, err := l.Append(w.sign(t, ev(seq))); err != nil {
			t.Fatal(err)
		}
		e := ev(seq * 10)
		e.Producer = "records"
		if _, err := l.Append(w.sign(t, e)); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := l.Since(Cursor{"north/desk": 2}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	c := Cursor{"north/desk": 2}
	for _, r := range recs {
		got = append(got, r.Ref().String())
		c.Advance(r.Ref())
	}
	want := "north/desk:3 north/desk:4 records:10 records:20 records:30 records:40"
	if strings.Join(got, " ") != want {
		t.Fatalf("since:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if c.String() != "north/desk=4,records=40" {
		t.Fatalf("cursor after reading %v", c)
	}
	more, _ := l.Since(c, 0)
	if len(more) != 0 {
		t.Fatalf("nothing past the heads, got %d", len(more))
	}
	lim, _ := l.Since(nil, 3)
	if len(lim) != 3 {
		t.Fatalf("limit 3 gave %d", len(lim))
	}
}

// A crash mid-append leaves a torn last line; opening the log drops it, so
// the producer can send that record again. A bad line anywhere else is
// corruption, and the log refuses to open.
func TestLogTornTail(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	l := openLog(t, dir, w)
	for seq := uint64(1); seq <= 2; seq++ {
		if _, err := l.Append(w.sign(t, ev(seq))); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	if len(files) != 1 {
		t.Fatalf("one file per producer, got %v", files)
	}
	line, _ := w.sign(t, ev(3)).Line()
	f, _ := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0)
	f.Write(line[:len(line)/2])
	f.Close()

	l2 := openLog(t, dir, w)
	if got := l2.Heads().String(); got != "north/desk=2" {
		t.Fatalf("heads after a torn tail %q", got)
	}
	if added, err := l2.Append(w.sign(t, ev(3))); err != nil || !added {
		t.Fatalf("resend after a torn tail: %v %v", added, err)
	}
	l2.Close()

	b, _ := os.ReadFile(files[0])
	lines := bytes.SplitAfter(b, []byte("\n"))
	lines[0] = []byte("{\"event\":null}\n")
	os.WriteFile(files[0], bytes.Join(lines, nil), 0o600)
	if _, err := OpenLog(dir, w.keys); err == nil {
		t.Fatal("a corrupt log opened")
	}
}

// Two writers on one log directory (two processes, say) can't both append
// the same seq: the second sees the first's record.
func TestLogTwoHandles(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	a := openLog(t, dir, w)
	b := openLog(t, dir, w)
	if added, err := a.Append(w.sign(t, ev(1))); err != nil || !added {
		t.Fatal(added, err)
	}
	fork := ev(1)
	fork.Kind = "job.done"
	if _, err := b.Append(w.sign(t, fork)); !errors.Is(err, ErrFork) {
		t.Fatalf("the second handle forked seq 1: %v", err)
	}
	if added, err := b.Append(w.sign(t, ev(2))); err != nil || !added {
		t.Fatal(added, err)
	}
	if got := a.Heads().String(); got != "north/desk=2" {
		t.Fatalf("the first handle's heads %q", got)
	}
}

// A record's signature is the format ssh-keygen checks, so a record can be
// verified by hand: the canonical bytes, the namespace, the pinned key.
func TestSignatureCheckableBySSHKeygen(t *testing.T) {
	kg, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	w := newWorld(t)
	r := w.sign(t, ev(1))
	dir := t.TempDir()
	msg, _ := r.Event.Canonical()
	signers := filepath.Join(dir, "signers")
	os.WriteFile(signers, []byte("north/desk "+w.seat.Public.String()+"\n"), 0o600)
	sig := filepath.Join(dir, "sig")
	os.WriteFile(sig, []byte(r.Sig), 0o600)
	cmd := exec.Command(kg, "-Y", "verify", "-f", signers, "-I", "north/desk", "-n", NSEvent, "-s", sig)
	cmd.Stdin = bytes.NewReader(msg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
}
