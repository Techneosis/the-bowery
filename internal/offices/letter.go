package offices

import (
	"bytes"
	"fmt"
	"strings"
	"time"
)

// A letter is what the placement transport carries: the placement block
// (the canonical bytes), the requester's signature over it, and, for a
// placement from another region, that region's seal over the same bytes.
//
//	```placement
//	asked_by = "alice"
//	...
//	```
//	```signature requester
//	-----BEGIN SSH SIGNATURE-----
//	...
//	-----END SSH SIGNATURE-----
//	```
//	```signature region
//	...
//	```
//
// Each signature is an `ssh-keygen -Y sign` signature, so a letter can be
// checked by hand: cut the block out and run `ssh-keygen -Y verify -n
// placement` against the office's signers file.

// SealLetter joins a block and its signatures into one letter. region is
// nil for a placement within the region.
func SealLetter(block, requester, region []byte) []byte {
	var b bytes.Buffer
	b.Write(block)
	for _, s := range []struct {
		name string
		sig  []byte
	}{{"requester", requester}, {"region", region}} {
		if s.sig == nil {
			continue
		}
		b.WriteString("```signature " + s.name + "\n")
		b.Write(bytes.TrimRight(s.sig, "\n"))
		b.WriteString("\n```\n")
	}
	return b.Bytes()
}

// SplitLetter cuts a letter into its block and its signatures, strictly:
// the block first, then at most one requester and one region signature,
// and nothing else.
func SplitLetter(b []byte) (block []byte, sigs map[string][]byte, err error) {
	s := string(b)
	if !strings.HasPrefix(s, fenceOpen+"\n") {
		return nil, nil, fmt.Errorf("not one placement block")
	}
	end := strings.Index(s, "\n"+fenceClose+"\n")
	if end < 0 {
		return nil, nil, fmt.Errorf("not one placement block")
	}
	cut := end + len("\n"+fenceClose+"\n")
	block, rest := []byte(s[:cut]), s[cut:]
	sigs = map[string][]byte{}
	for rest != "" {
		head, after, ok := strings.Cut(rest, "\n")
		name, isSig := strings.CutPrefix(head, "```signature ")
		if !ok || !isSig || (name != "requester" && name != "region") {
			return nil, nil, fmt.Errorf("something after the placement block that isn't a signature")
		}
		if sigs[name] != nil {
			return nil, nil, fmt.Errorf("two %s signatures", name)
		}
		body, next, ok := strings.Cut(after, "\n"+fenceClose+"\n")
		if !ok || strings.Contains(body, "```") {
			return nil, nil, fmt.Errorf("a signature that doesn't end")
		}
		sigs[name] = []byte(body + "\n")
		rest = next
	}
	return block, sigs, nil
}

// ---------------------------------------------------------------- signers

// Signer is one line of an office's signers file, which is ssh-keygen's
// allowed_signers format: `principals [namespaces="..."] ssh-ed25519 <key>
// [comment]`. A principal from another region is written
// `<principal>@<region>`; one of the office's own region is bare.
type Signer struct {
	Principals []string
	Namespaces []string // empty: any
	Key        PublicKey
	Problem    string // why this line can never verify anything
}

func parseSigners(path string, lines []string) []Signer {
	var out []Signer
	for _, line := range lines {
		toks, err := splitQuoted(line)
		s := Signer{}
		if err != nil || len(toks) == 0 {
			s.Problem = "a line that doesn't parse"
			out = append(out, s)
			continue
		}
		s.Principals = strings.Split(toks[0], ",")
		rest := toks[1:]
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "ssh-") && !strings.HasPrefix(rest[0], "ecdsa-") &&
			!strings.HasPrefix(rest[0], "sk-") {
			for _, opt := range splitOutsideQuotes(rest[0], ',') {
				k, v, _ := strings.Cut(opt, "=")
				switch strings.ToLower(k) {
				case "namespaces":
					s.Namespaces = strings.Split(strings.Trim(v, `"`), ",")
				default:
					s.Problem = fmt.Sprintf("option %s isn't supported here (only namespaces)", k)
				}
			}
			rest = rest[1:]
		}
		for _, p := range s.Principals {
			if strings.ContainsAny(p, "*?!") {
				s.Problem = "principal patterns aren't supported: name each principal"
			}
		}
		if s.Problem == "" {
			if len(rest) < 2 {
				s.Problem = "no key"
			} else if k, err := ParsePublicKey(rest[0] + " " + rest[1]); err != nil {
				s.Problem = err.Error()
			} else {
				s.Key = k
			}
		}
		out = append(out, s)
	}
	return out
}

// splitOutsideQuotes splits on sep, keeping a double-quoted run whole: the
// commas in namespaces="a,b" belong to the value, not to the option list.
func splitOutsideQuotes(s string, sep rune) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
		case r == sep && !inQ:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, cur.String())
}

// splitQuoted splits on blanks, keeping a double-quoted run whole.
func splitQuoted(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQ, any := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
			any = true
		case (r == ' ' || r == '\t') && !inQ:
			if any {
				out = append(out, cur.String())
				cur.Reset()
				any = false
			}
		default:
			cur.WriteRune(r)
			any = true
		}
	}
	if inQ {
		return nil, fmt.Errorf("an unclosed quote")
	}
	if any {
		out = append(out, cur.String())
	}
	return out, nil
}

// SignerPrincipal is the name a job's requester is listed under in an
// office of region `region`: bare for its own region, `name@region` for a
// peer's.
func SignerPrincipal(j *Job, region string) string {
	if j.From == "" || j.From == region {
		return j.AskedBy
	}
	return j.AskedBy + "@" + j.From
}

// ---------------------------------------------------------------- the office's checks

// VerifyRequester checks the requester's signature over the block against
// the office's signers file (spec §8.1). A false return is a drop: the
// placement isn't signed by a listed signer, and nobody is told.
func (c *Config) VerifyRequester(j *Job, block []byte, sig []byte) (PublicKey, string) {
	if sig == nil {
		return PublicKey{}, "it carries no requester's signature"
	}
	key, err := VerifySignature(sig, NSPlacement, block)
	if err != nil {
		return PublicKey{}, "its requester's signature doesn't verify"
	}
	who := SignerPrincipal(j, c.Region.Region)
	for _, s := range c.Office.SignerList {
		if s.Problem != "" || !contains(s.Principals, who) || !s.Key.Equal(key) {
			continue
		}
		if len(s.Namespaces) > 0 && !contains(s.Namespaces, NSPlacement) {
			continue
		}
		j.signer = &key
		return key, ""
	}
	return PublicKey{}, "it isn't signed by a listed signer"
}

// PeerProblem is why a placement from another region can't come in: the
// region isn't a peer, its seal isn't by the key this region pins, or the
// peer may not place into this office (spec §6, §8.2). "" means it may.
func (c *Config) PeerProblem(j *Job, block []byte, seal []byte) string {
	from := j.From
	p, ok := c.Region.Peers[from]
	if !ok {
		return fmt.Sprintf("region %s isn't a peer of region %s", from, c.Region.Region)
	}
	pin, err := ParsePublicKey(p.Key)
	if err != nil {
		return fmt.Sprintf("region %s's pinned key isn't usable, so there is no link", from)
	}
	if seal == nil {
		return fmt.Sprintf("the placement from region %s carries no region seal", from)
	}
	key, err := VerifySignature(seal, NSSeal, block)
	if err != nil {
		return fmt.Sprintf("the placement from region %s has a seal that doesn't verify", from)
	}
	if !key.Equal(pin) {
		return fmt.Sprintf("the placement from region %s is sealed by %s, not by the key region %s pins for it", from,
			key.Fingerprint(), c.Region.Region)
	}
	if !contains(p.MayPlace, c.Office.Name) {
		return fmt.Sprintf("region %s may not place into %s", from, c.Office.Name)
	}
	// Another region's clock: a signing time ahead of this one would stretch
	// max_expires (counted from it) and vouch for a moment that hasn't come.
	if sa, err := time.Parse(time.RFC3339Nano, j.SignedAt); err == nil && sa.Sub(time.Now()) > ClockSkew {
		return fmt.Sprintf("the placement from region %s is signed %s ahead of this clock", from, sa.Sub(time.Now()).Round(time.Second))
	}
	j.seal = &key
	return ""
}

// PeerLink is the live check on a job from another region, run in every
// evaluation (spec §6, "revoking is one edit"): the peer entry is still
// there, still lets it place here, and still pins the key that sealed it.
// "" means the link stands.
func (c *Config) PeerLink(j *Job) string {
	if j == nil || j.From == "" || j.From == c.Region.Region {
		return ""
	}
	p, ok := c.Region.Peers[j.From]
	if !ok {
		return fmt.Sprintf("region %s is no longer a peer of region %s", j.From, c.Region.Region)
	}
	if !contains(p.MayPlace, c.Office.Name) {
		return fmt.Sprintf("region %s may no longer place into %s", j.From, c.Office.Name)
	}
	pin, err := ParsePublicKey(p.Key)
	if err != nil || j.seal == nil || !j.seal.Equal(pin) {
		return fmt.Sprintf("the placement from region %s isn't sealed by the key region %s now pins for it", j.From, c.Region.Region)
	}
	return ""
}

// ReadLetter decodes a letter this office already accepted and re-reads its
// seal, so the run phase can check the peer link against today's pin.
func (c *Config) ReadLetter(b []byte) (*Job, error) {
	block, sigs, err := SplitLetter(b)
	if err != nil {
		return nil, err
	}
	j, err := Decode(block)
	if err != nil {
		return nil, err
	}
	if j.From != "" && j.From != c.Region.Region && sigs["region"] != nil {
		if key, err := VerifySignature(sigs["region"], NSSeal, block); err == nil {
			j.seal = &key
		}
	}
	return j, nil
}
