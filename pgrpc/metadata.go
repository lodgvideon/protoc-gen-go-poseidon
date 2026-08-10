package pgrpc

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
)

// binSuffix marks a key whose value is binary and therefore base64-encoded.
const binSuffix = "-bin"

// binEncoding must match poseidon's, which is base64.StdEncoding — padded on
// emit. The gRPC specification requires implementations to ACCEPT padded and
// unpadded alike, so matching the transport is what matters here.
var binEncoding = base64.StdEncoding

// binSpan locates one entry's base64 bytes inside Metadata's arena. n < 0 means
// the entry is textual and its value is not in the arena at all.
type binSpan struct{ off, n int }

// Metadata is a request-metadata builder that is allocation-free IN THE STEADY
// STATE. The first use of each distinct key interns its lowercased name, and
// the base64 arena grows once to its high-water mark; after that, rebuilding
// the same shape costs nothing.
//
// grpc.AppendMetadata does strings.ToLower(key) and []byte(k) on EVERY call, so
// building metadata per RPC allocates per entry per RPC. This type pays that
// once per distinct key.
//
// WHEN IT PAYS FOR ITSELF: only for metadata that CHANGES per RPC — a rotating
// token, a per-request id, a varying tenant. CONSTANT metadata should be built
// once with grpc.AppendMetadata and passed through WithMetadata forever; this
// builder buys nothing there and costs a type.
//
// It is NOT a validation bypass. Interning goes through grpc.AppendMetadata
// itself, so poseidon's own name, pseudo-header and reserved-key rules run on
// every distinct key — just once rather than once per RPC. Values are checked
// by poseidon inside NewStream, which re-validates every field of a
// caller-built slice precisely because such a slice never went through
// AppendMetadata.
//
// NOT SAFE FOR CONCURRENT USE. Build one at setup and share it read-only, or
// keep one per goroutine. Never call a mutator while another goroutine has an
// RPC in flight that was given this builder's Fields(): Fields aliases the
// builder's memory, and poseidon copies the field bytes into its HPACK encoder
// during SendHeaders inside NewStream, so a concurrent mutation races that copy
// and can ship a torn value — or another goroutine's credential.
//
// The whole "grpc-" namespace is refused by poseidon, so grpc-trace-bin and
// grpc-tags-bin cannot be sent through this type or any other. Tracked upstream
// as lodgvideon/poseidon-http-client#463.
//
// The zero value is ready to use.
type Metadata struct {
	fields []conn.HeaderField
	// spans is parallel to fields and locates binary values inside bin.
	spans []binSpan
	// names interns lowercased key bytes. It SURVIVES Reset, which is the
	// entire reason this builder is cheaper than grpc.AppendMetadata.
	names map[string][]byte
	// bin is the base64 arena, reused across Reset.
	bin []byte
}

// intern returns the lowercased name bytes for key, validating the key through
// poseidon's own rules the first time it is seen.
//
// Routing the check through grpc.AppendMetadata rather than reimplementing it
// is deliberate: poseidon's validMetadataName and checkMetadataKey are
// unexported, and a hand-rolled copy would drift the moment poseidon adds a
// reserved header. The cost is one allocation per DISTINCT key, ever.
func (m *Metadata) intern(key string) ([]byte, error) {
	if b, ok := m.names[key]; ok {
		return b, nil
	}
	probe, err := grpc.AppendMetadata(nil, key, nil)
	if err != nil {
		return nil, err
	}
	name := probe[0].Name
	if m.names == nil {
		m.names = make(map[string][]byte, 8)
	}
	m.names[key] = name
	return name, nil
}

// hasBinSuffixFold reports whether key ends in "-bin", case-insensitively,
// without allocating.
//
// strings.ToLower(key) would be the obvious spelling and is free for a key that
// is ALREADY lowercase — it returns the input unchanged. It allocates a new
// string for anything else, so a caller passing "X-Tenant" would pay one
// allocation per RPC for a check that never needed the lowercased form.
// Measured, not assumed: TestMetadataIsAllocationFreeInSteadyState uses a
// mixed-case key for exactly this reason.
func hasBinSuffixFold(key string) bool {
	if len(key) < len(binSuffix) {
		return false
	}
	return strings.EqualFold(key[len(key)-len(binSuffix):], binSuffix)
}

// checkText rejects a text value for a key that must carry binary.
//
// This is the ONE check this builder has to perform itself. poseidon validates
// a name, a reserved key, and a value's bytes, but nothing verifies that a
// "-bin" value is actually base64 — so an ASCII-clean raw value passes every
// gate on the wire and the server base64-decodes garbage.
// grpc.AppendMetadata cannot produce that shape, because it routes on the
// suffix; a builder that skips AppendMetadata must not end up less safe.
func checkText(key string) error {
	if hasBinSuffixFold(key) {
		return fmt.Errorf("pgrpc: key %q ends in %s; use SetBin or AddBin, which base64-encode the value",
			key, binSuffix)
	}
	return nil
}

// indexOf returns the position of the first entry named name, or -1.
func (m *Metadata) indexOf(name []byte) int {
	for i := range m.fields {
		// The interned name is one shared slice per key, so comparing the data
		// pointers would work — but only until someone hand-builds a field.
		// String comparison of byte slices does not allocate.
		if string(m.fields[i].Name) == string(name) {
			return i
		}
	}
	return -1
}

// put installs an entry, replacing the first one with the same name when
// replace is set and appending otherwise.
func (m *Metadata) put(name, value []byte, span binSpan, idx IndexingMode, replace bool) {
	f := conn.HeaderField{Name: name, Value: value, Indexing: idx}
	if replace {
		if i := m.indexOf(name); i >= 0 {
			// The previous binary value's arena bytes are orphaned until Reset.
			// Compacting them would invalidate every span after it for no gain:
			// the arena is bounded by one build's high-water mark either way.
			m.fields[i], m.spans[i] = f, span
			return
		}
	}
	m.fields = append(m.fields, f)
	m.spans = append(m.spans, span)
}

// textSpan marks an entry as non-binary.
var textSpan = binSpan{n: -1}

// SetText sets a text-valued entry, replacing any existing entry with the same
// key.
//
// value ALIASES the caller's slice until the RPC's headers are written, exactly
// as grpc.AppendMetadata does for text.
func (m *Metadata) SetText(key string, value []byte) error {
	return m.text(key, value, IndexIncremental, true)
}

// AddText appends a text-valued entry WITHOUT replacing an existing one with
// the same key. gRPC metadata permits repeated keys, and Set semantics alone
// cannot express them.
func (m *Metadata) AddText(key string, value []byte) error {
	return m.text(key, value, IndexIncremental, false)
}

// SetSensitive is SetText with Indexing: IndexNever, keeping the value out of
// the connection's HPACK dynamic table.
//
// poseidon marks authorization, proxy-authorization and cookie never-indexed
// automatically, but that is a floor. Any other credential-bearing header needs
// this — without it a bearer token is indexed once and then emitted as a
// one-byte reference on every later call over the same connection.
//
// poseidon's own docs say to set a "Sensitive" field on the header. That field
// does not exist: hpack.HeaderField has Name, Value and Indexing. Reported
// upstream as lodgvideon/poseidon-http-client#464.
func (m *Metadata) SetSensitive(key string, value []byte) error {
	return m.text(key, value, IndexNever, true)
}

func (m *Metadata) text(key string, value []byte, idx IndexingMode, replace bool) error {
	if err := checkText(key); err != nil {
		return err
	}
	name, err := m.intern(key)
	if err != nil {
		return err
	}
	m.put(name, value, textSpan, idx, replace)
	return nil
}

// SetBin sets a binary-valued entry, base64-encoding value into memory this
// builder owns and reuses. It replaces any existing entry with the same key.
//
// ARENA REUSE. The encoding lands in a reused arena, so a binary value obtained
// through Fields() is INVALIDATED by the next Reset. grpc.AppendMetadata
// encodes into fresh memory and carries no such constraint — a real advantage
// for anything long-lived.
func (m *Metadata) SetBin(key string, value []byte) error {
	return m.binary(key, value, true)
}

// AddBin appends a binary-valued entry without replacing an existing one.
func (m *Metadata) AddBin(key string, value []byte) error {
	return m.binary(key, value, false)
}

func (m *Metadata) binary(key string, value []byte, replace bool) error {
	name, err := m.intern(key)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(string(name), binSuffix) {
		return fmt.Errorf("pgrpc: key %q does not end in %s; use SetText or AddText",
			key, binSuffix)
	}
	off := len(m.bin)
	m.bin = binEncoding.AppendEncode(m.bin, value)
	// The Value is filled in by Fields, not here. Appending to the arena can
	// REALLOCATE it, and a value bound now would keep pointing at the old
	// array — which stays perfectly readable, Go having no dangling pointers,
	// so this is NOT about corruption. It is about memory: every entry bound to
	// an abandoned arena PINS that arena, so a build that grows the arena k
	// times holds k generations alive and the reuse this whole type exists for
	// stops happening. The span is the durable record.
	m.put(name, nil, binSpan{off: off, n: len(m.bin) - off}, IndexIncremental, replace)
	return nil
}

// Fields returns the built slice, for WithMetadata or for CallConfig.SetMetadata.
//
// It aliases this builder and is invalidated by the next mutation or Reset.
//
// Binary values are bound to the arena here rather than when they were set, so
// that every value points into the CURRENT arena. Binding at set time would
// still read back correctly — the abandoned array survives as long as anything
// references it — but each entry would pin the generation it was bound to, and
// the arena would stop being reused. See the comment in binary.
func (m *Metadata) Fields() []conn.HeaderField {
	for i := range m.spans {
		if s := m.spans[i]; s.n >= 0 {
			end := s.off + s.n
			m.fields[i].Value = m.bin[s.off:end:end]
		}
	}
	return m.fields
}

// Reset prepares the builder for another RPC.
//
// It CLEARS the field entries rather than merely truncating, the same rule
// poseidon's putHeaderScratch follows: those entries point at caller memory
// that for gRPC routinely means credentials, and a reused buffer is the wrong
// place to leave them reachable past the RPC that carried them.
//
// It KEEPS the interned names and the arena's capacity, which is what makes the
// next build free.
func (m *Metadata) Reset() {
	clear(m.fields)
	m.fields = m.fields[:0]
	m.spans = m.spans[:0]
	// The arena holds base64 of credentials just as often as the fields do.
	clear(m.bin)
	m.bin = m.bin[:0]
}

// Get reads a value out of response metadata, decoding a "-bin" key. It is a
// thin pass-through to grpc.MetadataValue.
//
// ok and err are independent, and deliberately so: a corrupted binary value
// returns (nil, true, err), never (nil, false, nil). Folding the two together
// would make "the peer sent nothing" indistinguishable from "the peer sent
// something deliberately corrupt", and an application reading a signature out
// of metadata would then take its no-credential-required branch on a value the
// peer controls.
func Get(md []conn.HeaderField, key string) (value []byte, ok bool, err error) {
	return grpc.MetadataValue(md, key)
}
