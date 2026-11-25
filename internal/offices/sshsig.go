package offices

// SSH signatures in the format `ssh-keygen -Y sign` writes and
// `ssh-keygen -Y verify` checks (OpenSSH's PROTOCOL.sshsig), for ssh-ed25519
// keys, with the standard library alone. A placement, a region's seal and a
// catalog are each signed this way, under their own namespace, so a
// signature made for one can never pass as another.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The namespaces: a requester signs a placement, a region seals one it sends
// to a peer, and a region signs the catalog it publishes to a peer.
const (
	NSPlacement = "placement"
	NSSeal      = "placement-region"
	NSCatalog   = "offices-catalog"
)

const (
	sigMagic   = "SSHSIG"
	sigBegin   = "-----BEGIN SSH SIGNATURE-----"
	sigEnd     = "-----END SSH SIGNATURE-----"
	keyEd25519 = "ssh-ed25519"
)

// PublicKey is one ssh-ed25519 public key, as an authorized_keys line
// writes it.
type PublicKey struct {
	Key ed25519.PublicKey
}

func (k PublicKey) blob() []byte {
	var b bytes.Buffer
	putString(&b, []byte(keyEd25519))
	putString(&b, k.Key)
	return b.Bytes()
}

// String is the key as `ssh-ed25519 <base64>`.
func (k PublicKey) String() string {
	return keyEd25519 + " " + base64.StdEncoding.EncodeToString(k.blob())
}

// Fingerprint is the key's SHA256 fingerprint, as ssh-keygen -l prints it.
func (k PublicKey) Fingerprint() string {
	sum := sha256.Sum256(k.blob())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func (k PublicKey) Equal(o PublicKey) bool { return bytes.Equal(k.Key, o.Key) }

// ParsePublicKey reads `ssh-ed25519 <base64> [comment]`. Any other key type
// is refused: this core verifies ed25519 signatures only.
func ParsePublicKey(s string) (PublicKey, error) {
	f := strings.Fields(s)
	if len(f) < 2 {
		return PublicKey{}, errors.New("want `ssh-ed25519 <base64> [comment]`")
	}
	if f[0] != keyEd25519 {
		return PublicKey{}, fmt.Errorf("key type %s: only ssh-ed25519 keys are verified", f[0])
	}
	raw, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return PublicKey{}, errors.New("the key isn't base64")
	}
	return parseKeyBlob(raw)
}

func parseKeyBlob(raw []byte) (PublicKey, error) {
	r := &reader{b: raw}
	typ, key := r.str(), r.str()
	if r.err != nil || len(r.b) != 0 || string(typ) != keyEd25519 || len(key) != ed25519.PublicKeySize {
		return PublicKey{}, errors.New("not an ssh-ed25519 key")
	}
	return PublicKey{Key: ed25519.PublicKey(bytes.Clone(key))}, nil
}

// signedData is what an SSH signature actually signs: the namespace and a
// hash of the message, never the message itself.
func signedData(namespace, hashAlg string, msg []byte) ([]byte, error) {
	var h []byte
	switch hashAlg {
	case "sha512":
		s := sha512.Sum512(msg)
		h = s[:]
	case "sha256":
		s := sha256.Sum256(msg)
		h = s[:]
	default:
		return nil, fmt.Errorf("hash %q", hashAlg)
	}
	var b bytes.Buffer
	b.WriteString(sigMagic)
	putString(&b, []byte(namespace))
	putString(&b, nil)
	putString(&b, []byte(hashAlg))
	putString(&b, h)
	return b.Bytes(), nil
}

// Sign makes an armored SSH signature over msg in namespace.
func (k PrivateKey) Sign(namespace string, msg []byte) []byte {
	if k.external != "" {
		return nil // never: SignWith handles external keys
	}
	data, _ := signedData(namespace, "sha512", msg)
	sig := ed25519.Sign(k.key, data)
	var sb bytes.Buffer
	putString(&sb, []byte(keyEd25519))
	putString(&sb, sig)
	var b bytes.Buffer
	b.WriteString(sigMagic)
	_ = binary.Write(&b, binary.BigEndian, uint32(1))
	putString(&b, k.Public.blob())
	putString(&b, []byte(namespace))
	putString(&b, nil)
	putString(&b, []byte("sha512"))
	putString(&b, sb.Bytes())
	return armor(b.Bytes())
}

func armor(raw []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(raw)
	var b strings.Builder
	b.WriteString(sigBegin + "\n")
	for len(enc) > 70 {
		b.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	b.WriteString(enc + "\n" + sigEnd + "\n")
	return []byte(b.String())
}

// VerifySignature checks an armored SSH signature over msg in namespace and
// returns the key that made it. The caller decides whether that key is one
// it trusts.
func VerifySignature(armored []byte, namespace string, msg []byte) (PublicKey, error) {
	s := strings.TrimSpace(string(armored))
	if !strings.HasPrefix(s, sigBegin) || !strings.HasSuffix(s, sigEnd) {
		return PublicKey{}, errors.New("not an armored SSH signature")
	}
	body := strings.Join(strings.Fields(s[len(sigBegin):len(s)-len(sigEnd)]), "")
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return PublicKey{}, errors.New("the signature isn't base64")
	}
	if !bytes.HasPrefix(raw, []byte(sigMagic)) {
		return PublicKey{}, errors.New("not an SSHSIG signature")
	}
	r := &reader{b: raw[len(sigMagic):]}
	version := r.u32()
	pub, ns, _, hashAlg, sigBlob := r.str(), r.str(), r.str(), r.str(), r.str()
	if r.err != nil || len(r.b) != 0 {
		return PublicKey{}, errors.New("a malformed signature")
	}
	if version != 1 {
		return PublicKey{}, fmt.Errorf("signature version %d", version)
	}
	if string(ns) != namespace {
		return PublicKey{}, fmt.Errorf("signed for namespace %q, not %q", ns, namespace)
	}
	key, err := parseKeyBlob(pub)
	if err != nil {
		return PublicKey{}, err
	}
	sr := &reader{b: sigBlob}
	typ, sig := sr.str(), sr.str()
	if sr.err != nil || len(sr.b) != 0 || string(typ) != keyEd25519 || len(sig) != ed25519.SignatureSize {
		return PublicKey{}, errors.New("not an ssh-ed25519 signature")
	}
	data, err := signedData(namespace, string(hashAlg), msg)
	if err != nil {
		return PublicKey{}, err
	}
	if !ed25519.Verify(key.Key, data, sig) {
		return PublicKey{}, errors.New("the signature doesn't match")
	}
	return key, nil
}

// PrivateKey is a signing key: an unencrypted OpenSSH ed25519 private key
// read from its file, or (for a key with a passphrase, or one held in an
// agent) a key file handed to `ssh-keygen -Y sign`.
type PrivateKey struct {
	Public   PublicKey
	key      ed25519.PrivateKey
	external string // the file ssh-keygen signs with
}

// LoadPrivateKey reads a private key file. The file must be readable by its
// owner alone, as ssh itself insists. A key this code can't read itself (a
// passphrase, an agent-held key named by its .pub file) is signed with by
// ssh-keygen, which can ask the agent.
func LoadPrivateKey(path string) (PrivateKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return PrivateKey{}, err
	}
	if fi.Mode().Perm()&0o077 != 0 && !strings.HasSuffix(path, ".pub") {
		return PrivateKey{}, fmt.Errorf("%s: a private key must be readable by its owner alone (it is %04o)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return PrivateKey{}, err
	}
	if k, err := parseOpenSSHPrivate(b); err == nil {
		return k, nil
	} else if !errors.Is(err, errNeedsSSHKeygen) {
		return PrivateKey{}, fmt.Errorf("%s: %v", path, err)
	}
	// ssh-keygen signs; the public half says which key it is.
	pubPath := path
	if !strings.HasSuffix(pubPath, ".pub") {
		pubPath += ".pub"
	}
	pb, err := os.ReadFile(pubPath)
	if err != nil {
		return PrivateKey{}, fmt.Errorf("%s: an encrypted or agent-held key needs its .pub beside it: %v", path, err)
	}
	pub, err := ParsePublicKey(string(pb))
	if err != nil {
		return PrivateKey{}, fmt.Errorf("%s: %v", pubPath, err)
	}
	return PrivateKey{Public: pub, external: path}, nil
}

var errNeedsSSHKeygen = errors.New("needs ssh-keygen")

// SignWith signs msg in namespace with k, natively or through ssh-keygen.
func SignWith(k PrivateKey, namespace string, msg []byte) ([]byte, error) {
	if k.external == "" {
		return k.Sign(namespace, msg), nil
	}
	cmd := exec.Command("ssh-keygen", "-q", "-Y", "sign", "-f", k.external, "-n", namespace)
	cmd.Stdin = bytes.NewReader(msg)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ssh-keygen -Y sign with %s: %v: %s", filepath.Base(k.external), err, strings.TrimSpace(stderr.String()))
	}
	got, err := VerifySignature(out, namespace, msg)
	if err != nil || !got.Equal(k.Public) {
		return nil, fmt.Errorf("ssh-keygen's signature with %s doesn't verify", filepath.Base(k.external))
	}
	return out, nil
}

// parseOpenSSHPrivate reads an unencrypted `openssh-key-v1` ed25519 key.
func parseOpenSSHPrivate(pem []byte) (PrivateKey, error) {
	s := strings.TrimSpace(string(pem))
	const begin, end = "-----BEGIN OPENSSH PRIVATE KEY-----", "-----END OPENSSH PRIVATE KEY-----"
	if !strings.HasPrefix(s, begin) || !strings.HasSuffix(s, end) {
		if strings.HasPrefix(s, keyEd25519+" ") {
			return PrivateKey{}, errNeedsSSHKeygen // a .pub: the agent holds the key
		}
		return PrivateKey{}, errors.New("not an OpenSSH private key")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s[len(begin):len(s)-len(end)]), ""))
	if err != nil {
		return PrivateKey{}, errors.New("the key isn't base64")
	}
	const magic = "openssh-key-v1\x00"
	if !bytes.HasPrefix(raw, []byte(magic)) {
		return PrivateKey{}, errors.New("not an openssh-key-v1 key")
	}
	r := &reader{b: raw[len(magic):]}
	cipher, kdf, _ := r.str(), r.str(), r.str()
	n := r.u32()
	if r.err != nil {
		return PrivateKey{}, errors.New("a malformed key")
	}
	if string(cipher) != "none" || string(kdf) != "none" {
		return PrivateKey{}, errNeedsSSHKeygen
	}
	if n != 1 {
		return PrivateKey{}, errors.New("a key file with more than one key")
	}
	pubBlob, priv := r.str(), r.str()
	if r.err != nil {
		return PrivateKey{}, errors.New("a malformed key")
	}
	pub, err := parseKeyBlob(pubBlob)
	if err != nil {
		return PrivateKey{}, errors.New("not an ed25519 key: only ssh-ed25519 keys sign here")
	}
	pr := &reader{b: priv}
	c1, c2 := pr.u32(), pr.u32()
	typ, pk, sk := pr.str(), pr.str(), pr.str()
	if pr.err != nil || c1 != c2 || string(typ) != keyEd25519 || len(sk) != ed25519.PrivateKeySize || !bytes.Equal(pk, pub.Key) {
		return PrivateKey{}, errors.New("a malformed ed25519 key")
	}
	key := ed25519.PrivateKey(bytes.Clone(sk))
	if !bytes.Equal(key.Public().(ed25519.PublicKey), pub.Key) {
		return PrivateKey{}, errors.New("the key's halves don't match")
	}
	return PrivateKey{Public: pub, key: key}, nil
}

// ---------------------------------------------------------------- wire

func putString(b *bytes.Buffer, s []byte) {
	_ = binary.Write(b, binary.BigEndian, uint32(len(s)))
	b.Write(s)
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) u32() uint32 {
	if r.err != nil || len(r.b) < 4 {
		r.err = errors.New("short")
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) str() []byte {
	n := r.u32()
	if r.err != nil || uint64(len(r.b)) < uint64(n) {
		r.err = errors.New("short")
		return nil
	}
	s := r.b[:n]
	r.b = r.b[n:]
	return s
}
