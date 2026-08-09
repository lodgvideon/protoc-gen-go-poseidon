// Package protocodec implements pgrpc.Codec over google.golang.org/protobuf.
//
// It is the only package in this module that imports protobuf's runtime, which
// is what lets a user on vtprotobuf alone avoid linking the reflection code —
// see the dependency note on vtcodec.Codec.Fallback for the one case where that
// property does not hold.
package protocodec

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

// Codec is the default pgrpc.Codec: google.golang.org/protobuf, with the
// marshal path routed through MarshalAppend so a caller-owned buffer is reused
// rather than reallocated per call.
//
// The zero value is ready to use.
type Codec struct {
	// MarshalOpts and UnmarshalOpts are named with the Opts suffix rather than
	// Marshal/Unmarshal because Go forbids a field and a method sharing a name
	// on one type, and this type has an Unmarshal method.
	MarshalOpts   proto.MarshalOptions
	UnmarshalOpts proto.UnmarshalOptions
}

// MarshalAppend appends the wire encoding of m to dst.
func (c Codec) MarshalAppend(dst []byte, m any) ([]byte, error) {
	pm, ok := m.(proto.Message)
	if !ok {
		return dst, fmt.Errorf("protocodec: %T is not a proto.Message", m)
	}
	return c.MarshalOpts.MarshalAppend(dst, pm)
}

// Unmarshal parses src into m, discarding m's previous contents.
func (c Codec) Unmarshal(src []byte, m any) error {
	pm, ok := m.(proto.Message)
	if !ok {
		return fmt.Errorf("protocodec: %T is not a proto.Message", m)
	}
	u := c.UnmarshalOpts
	// Documentation of intent rather than a behaviour change: Merge's zero
	// value is already false. The line is here so that someone setting
	// UnmarshalOpts.Merge = true on the struct cannot quietly violate
	// pgrpc.Codec.Unmarshal's reset-shaped contract, which callers rely on when
	// they reuse an out message across calls.
	u.Merge = false
	return u.Unmarshal(src, pm)
}

// Name returns the gRPC content-subtype for protobuf.
func (Codec) Name() string { return "proto" }
