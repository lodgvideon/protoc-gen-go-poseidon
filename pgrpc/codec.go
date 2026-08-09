package pgrpc

// Codec turns messages into gRPC message payloads and back.
//
// It is append-shaped on the marshal side: MarshalAppend writes into memory the
// caller already owns, which is what lets a load generator drive a steady
// stream of RPCs without a per-request allocation for the request body. That
// shape fits both google.golang.org/protobuf's
// proto.MarshalOptions.MarshalAppend and vtprotobuf's SizeVT /
// MarshalToSizedBufferVT pair.
//
// The message type is any rather than proto.Message deliberately, for two
// reasons. First, gogo-generated and golang/protobuf-v1 messages do not satisfy
// protoreflect.ProtoMessage, and forcing them through protoadapt.MessageV2Of
// would cost an allocation per call — the cost this package exists to avoid.
// Second, and more important: it keeps this package free of any protobuf
// import, so a user on vtprotobuf alone links no reflection code. The concrete
// type check lives inside the codec, which is the only thing that knows its
// message family.
//
// A Codec must be safe for concurrent use: one instance is shared by every RPC
// on a Client.
type Codec interface {
	// MarshalAppend appends the wire encoding of m to dst and returns the
	// extended slice. It must not retain dst. On error the returned slice is
	// unspecified and the caller must not use it.
	MarshalAppend(dst []byte, m any) ([]byte, error)

	// Unmarshal parses src into m, DISCARDING m's previous contents.
	//
	// The reset is contractual, not incidental. Callers reuse out messages
	// across calls — that is the entire point of the buffer-reusing path — and
	// a merge-shaped unmarshal would accumulate every prior response's repeated
	// and map fields into the current one. proto.Unmarshal resets; vtprotobuf's
	// UnmarshalVT does NOT, so a vt codec must reset first. See vtcodec.
	//
	// src aliases a buffer valid only for this call. An implementation that
	// retains bytes must copy them.
	Unmarshal(src []byte, m any) error

	// Name is the codec's gRPC content-subtype ("proto", "json", …).
	//
	// It cannot reach the wire today, and that is a poseidon limitation rather
	// than a choice here: the request content-type is the constant
	// "application/grpc" with no subtype, and grpc.AppendMetadata refuses
	// "content-type" as a reserved key, so there is no channel for it. Tracked
	// upstream as lodgvideon/poseidon-http-client#462. Until that lands, Name
	// is for diagnostics and error messages only.
	Name() string
}
