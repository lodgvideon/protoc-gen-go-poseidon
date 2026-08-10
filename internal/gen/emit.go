package gen

import (
	"google.golang.org/protobuf/compiler/protogen"
)

// streamKind is the runtime type a streaming method returns.
func streamKind(m *protogen.Method) string {
	switch {
	case m.Desc.IsStreamingClient() && m.Desc.IsStreamingServer():
		return "BidiStream"
	case m.Desc.IsStreamingServer():
		return "ServerStream"
	case m.Desc.IsStreamingClient():
		return "ClientStream"
	}
	return ""
}

// streamType emits the fully-qualified stream type for a streaming method,
// including its type arguments.
//
// The type arguments are the VALUE types, not pointers: the runtime's Recv
// takes *Resp and RecvNew returns *Resp, so the parameter is instantiated with
// the message type itself.
func (x *fileGen) streamType(m *protogen.Method) []any {
	out := []any{x.pkgs.pgrpc.Ident(streamKind(m)), "["}
	if m.Desc.IsStreamingClient() {
		out = append(out, m.Input.GoIdent, ", ")
	}
	return append(out, m.Output.GoIdent, "]")
}

// resultType emits a method's return type, without the trailing error.
func (x *fileGen) resultType(m *protogen.Method) []any {
	if streamKind(m) == "" {
		return []any{"*", m.Output.GoIdent}
	}
	return append([]any{"*"}, x.streamType(m)...)
}

// takesRequest reports whether the method's client-side entry point accepts a
// single request message. Client-streaming and bidirectional calls send theirs
// through the stream instead.
func takesRequest(m *protogen.Method) bool { return !m.Desc.IsStreamingClient() }

// service emits everything for one service.
func (x *fileGen) service(s *protogen.Service) {
	impl, err := unexport(s.GoName)
	if err != nil {
		x.names.conflicts = append(x.names.conflicts,
			"  service "+string(s.Desc.FullName())+": "+err.Error())
		return
	}

	x.methodConsts(s)
	x.streamAliases(s)
	x.clientInterface(s)
	x.clientImpl(s, impl)
	if x.cfg.Callers {
		x.caller(s)
	}
}

// methodConsts emits the full-method-name constants.
func (x *fileGen) methodConsts(s *protogen.Service) {
	if !x.cfg.MethodConsts || len(s.Methods) == 0 {
		return
	}
	g := x.g
	g.P("const (")
	for _, m := range s.Methods {
		name := x.names.claim(s.GoName+"_"+m.GoName+"_FullMethodName",
			"the full-method name of "+string(s.Desc.FullName())+"."+string(m.Desc.Name()))
		g.P(name, " = ", strconvQuote(fullMethod(s, m)))
	}
	g.P(")")
	g.P()
}

// streamAliases emits a readable name for each streaming method's stream type.
func (x *fileGen) streamAliases(s *protogen.Service) {
	for _, m := range s.Methods {
		if streamKind(m) == "" {
			continue
		}
		name := x.names.claim(s.GoName+"_"+m.GoName+"Client",
			"the stream alias for "+string(s.Desc.FullName())+"."+string(m.Desc.Name()))
		x.g.P("// ", name, " names the stream ", s.GoName, ".", m.GoName, " returns.")
		x.g.P("//")
		x.g.P("// It is a POINTER alias. The stream carries a mutex and three atomics and")
		x.g.P("// must never be copied; a value alias would let `var s ", name, "` compile")
		x.g.P("// and would turn passing one by value into a go vet copylocks finding in")
		x.g.P("// your build, not ours.")
		x.g.P(append([]any{"type ", name, " = *"}, x.streamType(m)...)...)
		x.g.P()
	}
}

// clientInterface emits the ergonomic face.
func (x *fileGen) clientInterface(s *protogen.Service) {
	if !x.cfg.Interfaces {
		return
	}
	g := x.g
	name := x.names.claim(s.GoName+"Client", "the client interface for "+string(s.Desc.FullName()))
	doc := name + " is the ergonomic face of the " + s.GoName + " service.\n" +
		"// It allocates a response message per call."
	// Only point at the Caller when one is actually emitted. Under
	// callers=false this doc would otherwise name a type that is not in the
	// generated package at all.
	if x.cfg.Callers {
		doc += "\n// For the buffer-reusing path see " + s.GoName + "Caller."
	}
	x.doc(doc, s.Comments.Leading)
	g.P("type ", name, " interface {")
	for _, m := range s.Methods {
		x.methodDoc(m)
		x.interfaceSignature(m)
	}
	g.P("}")
	g.P()
}

// interfaceSignature emits one method's signature, without a body.
func (x *fileGen) interfaceSignature(m *protogen.Method) {
	parts := []any{m.GoName, "(ctx ", x.pkgs.ctx.Ident("Context")}
	if takesRequest(m) {
		parts = append(parts, ", in *", m.Input.GoIdent)
	}
	parts = append(parts, ", opts ...", x.pkgs.pgrpc.Ident("CallOption"), ") (")
	parts = append(parts, x.resultType(m)...)
	parts = append(parts, ", error)")
	x.g.P(parts...)
}

// clientImpl emits the implementation struct, its constructors and its methods.
func (x *fileGen) clientImpl(s *protogen.Service, impl string) {
	g := x.g
	iface := s.GoName + "Client"
	implName := x.names.claim(impl+"Client", "the client implementation for "+string(s.Desc.FullName()))

	// The struct is unexported and the constructor returns the interface when
	// one is emitted, so the concrete type is not part of the generated API.
	g.P("type ", implName, " struct {")
	g.P("c *", x.pkgs.pgrpc.Ident("Client"))
	g.P("}")
	g.P()

	ret := any(iface)
	if !x.cfg.Interfaces {
		ret = any("*" + implName)
	} else {
		g.P("var _ ", iface, " = (*", implName, ")(nil)")
		g.P()
	}

	ctor := x.names.claim("New"+s.GoName+"Client", "the client constructor for "+string(s.Desc.FullName()))
	g.P("// ", ctor, " returns the ergonomic face of ", s.GoName, " over c.")
	g.P("func ", ctor, "(c *", x.pkgs.pgrpc.Ident("Client"), ") ", ret, " {")
	g.P("return &", implName, "{c: c}")
	g.P("}")
	g.P()

	if x.cfg.emitDefaultCodec() {
		on := x.names.claim(ctor+"On", "the codec-supplying client constructor for "+string(s.Desc.FullName()))
		g.P("// ", on, " builds a ", x.pkgs.pgrpc.Ident("Client"), " over cc with the protobuf codec and")
		g.P("// returns the ergonomic face of ", s.GoName, ".")
		g.P("//")
		g.P("// The default codec is applied BEFORE opts, so a caller-supplied")
		g.P("// pgrpc.WithCodec still wins. It exists because pgrpc.NewClient panics without")
		g.P("// a codec and this package cannot supply one: the runtime must not import")
		g.P("// protobuf, while this generated file already does.")
		g.P("func ", on, "(cc ", x.pkgs.pgrpc.Ident("Invoker"),
			", opts ...", x.pkgs.pgrpc.Ident("ClientOption"), ") ", ret, " {")
		g.P("return ", ctor, "(", x.pkgs.pgrpc.Ident("NewClient"), "(cc,")
		g.P("append([]", x.pkgs.pgrpc.Ident("ClientOption"), "{",
			x.pkgs.pgrpc.Ident("WithCodec"), "(", x.pkgs.protocodec.Ident("Codec"), "{})}, opts...)...))")
		g.P("}")
		g.P()
	}

	for _, m := range s.Methods {
		x.methodDoc(m)
		parts := []any{"func (x *", implName, ") "}
		parts = append(parts, m.GoName, "(ctx ", x.pkgs.ctx.Ident("Context"))
		if takesRequest(m) {
			parts = append(parts, ", in *", m.Input.GoIdent)
		}
		parts = append(parts, ", opts ...", x.pkgs.pgrpc.Ident("CallOption"), ") (")
		parts = append(parts, x.resultType(m)...)
		parts = append(parts, ", error) {")
		g.P(parts...)
		x.clientBody(s, m)
		g.P("}")
		g.P()
	}
}

// methodRef names the wire path at a call site: the constant when one was
// emitted, the literal otherwise.
func (x *fileGen) methodRef(s *protogen.Service, m *protogen.Method) string {
	if x.cfg.MethodConsts {
		return s.GoName + "_" + m.GoName + "_FullMethodName"
	}
	return strconvQuote(fullMethod(s, m))
}

// clientBody emits one interface-face method body.
func (x *fileGen) clientBody(s *protogen.Service, m *protogen.Method) {
	g := x.g
	ref := x.methodRef(s, m)

	switch streamKind(m) {
	case "":
		g.P("out := new(", m.Output.GoIdent, ")")
		g.P("if err := ", x.pkgs.pgrpc.Ident("UnaryOpts"), "(ctx, x.c, ", ref, ", in, out, opts...); err != nil {")
		g.P("return nil, err")
		g.P("}")
		g.P("return out, nil")
	case "ServerStream":
		parts := []any{"return ", x.pkgs.pgrpc.Ident("NewServerStreamOpts"), "["}
		parts = append(parts, m.Output.GoIdent, "](ctx, x.c, ", ref, ", in, opts...)")
		g.P(parts...)
	case "ClientStream":
		parts := []any{"return ", x.pkgs.pgrpc.Ident("NewClientStreamOpts"), "["}
		parts = append(parts, m.Input.GoIdent, ", ", m.Output.GoIdent, "](ctx, x.c, ", ref, ", opts...)")
		g.P(parts...)
	case "BidiStream":
		parts := []any{"return ", x.pkgs.pgrpc.Ident("NewBidiStreamOpts"), "["}
		parts = append(parts, m.Input.GoIdent, ", ", m.Output.GoIdent, "](ctx, x.c, ", ref, ", opts...)")
		g.P(parts...)
	}
}

// caller emits the buffer-reusing face.
func (x *fileGen) caller(s *protogen.Service) {
	g := x.g
	name := x.names.claim(s.GoName+"Caller", "the caller struct for "+string(s.Desc.FullName()))

	x.doc(name+" is the buffer-reusing face of the "+s.GoName+" service.\n"+
		"//\n"+
		"// One Caller serves ONE goroutine and ONE in-flight RPC. It owns the request\n"+
		"// marshal scratch, the response scratch and the resolved call configuration.\n"+
		"// The intended shape for a load generator is one Caller per virtual user,\n"+
		"// built once outside the request loop.\n"+
		"//\n"+
		"// A UNARY call allocates nothing in this layer. A STREAMING call allocates the\n"+
		"// stream and grows its send buffer once, so the Caller saves one resolved\n"+
		"// configuration per STREAM — not per message.\n"+
		"//\n"+
		"// CONFIG LIFETIME. Config is mutable and a call reads it SYNCHRONOUSLY, because\n"+
		"// poseidon walks the metadata inside NewStream. Mutate it outside the request\n"+
		"// loop only, never while a call or an open stream is using it.\n"+
		"//\n"+
		"// A concurrent second call fails with pgrpc.ErrCallerInUse rather than\n"+
		"// corrupting a request body.", s.Comments.Leading)

	g.P("type ", name, " struct {")
	g.P(x.pkgs.pgrpc.Ident("Guard"))
	g.P("c *", x.pkgs.pgrpc.Ident("Client"))
	g.P("cfg ", x.pkgs.pgrpc.Ident("CallConfig"))
	g.P("buf []byte")
	g.P("respBuf []byte")
	g.P("}")
	g.P()

	ctor := x.names.claim("New"+name, "the caller constructor for "+string(s.Desc.FullName()))
	g.P("// ", ctor, " returns the buffer-reusing face of ", s.GoName, " over c.")
	g.P("func ", ctor, "(c *", x.pkgs.pgrpc.Ident("Client"), ") *", name, " {")
	g.P("return &", name, "{c: c}")
	g.P("}")
	g.P()

	if x.cfg.emitDefaultCodec() {
		on := x.names.claim(ctor+"On", "the codec-supplying caller constructor for "+string(s.Desc.FullName()))
		g.P("// ", on, " builds a ", x.pkgs.pgrpc.Ident("Client"), " over cc with the protobuf codec and")
		g.P("// returns the buffer-reusing face of ", s.GoName, ". The default codec is applied")
		g.P("// BEFORE opts, so a caller-supplied pgrpc.WithCodec still wins.")
		g.P("func ", on, "(cc ", x.pkgs.pgrpc.Ident("Invoker"),
			", opts ...", x.pkgs.pgrpc.Ident("ClientOption"), ") *", name, " {")
		g.P("return ", ctor, "(", x.pkgs.pgrpc.Ident("NewClient"), "(cc,")
		g.P("append([]", x.pkgs.pgrpc.Ident("ClientOption"), "{",
			x.pkgs.pgrpc.Ident("WithCodec"), "(", x.pkgs.protocodec.Ident("Codec"), "{})}, opts...)...))")
		g.P("}")
		g.P()
	}

	g.P("// Config returns the mutable per-call configuration. Set it once, outside the")
	g.P("// request loop. Prefer Config().Apply(opts...) over assigning fields, so that")
	g.P("// option logic — metadata ownership above all — is not bypassed.")
	g.P("func (x *", name, ") Config() *", x.pkgs.pgrpc.Ident("CallConfig"), " { return &x.cfg }")
	g.P()

	for _, m := range s.Methods {
		x.methodDoc(m)
		x.callerMethod(s, m, name)
	}
}

// callerMethod emits one buffer-reusing method.
func (x *fileGen) callerMethod(s *protogen.Service, m *protogen.Method, recv string) {
	g := x.g
	ref := x.methodRef(s, m)
	unary := streamKind(m) == ""

	parts := []any{"func (x *", recv, ") ", m.GoName, "(ctx ", x.pkgs.ctx.Ident("Context")}
	if takesRequest(m) {
		parts = append(parts, ", in *", m.Input.GoIdent)
	}
	if unary {
		parts = append(parts, ", out *", m.Output.GoIdent, ") error {")
	} else {
		parts = append(parts, ") (")
		parts = append(parts, x.resultType(m)...)
		parts = append(parts, ", error) {")
	}
	g.P(parts...)

	g.P("if err := x.Enter(); err != nil {")
	if unary {
		g.P("return err")
	} else {
		g.P("return nil, err")
	}
	g.P("}")
	g.P("defer x.Leave()")

	switch streamKind(m) {
	case "":
		g.P("return ", x.pkgs.pgrpc.Ident("Unary"),
			"(ctx, x.c, &x.cfg, ", ref, ", in, out, &x.buf, &x.respBuf)")
	case "ServerStream":
		p := []any{"return ", x.pkgs.pgrpc.Ident("NewServerStream"), "["}
		p = append(p, m.Output.GoIdent, "](ctx, x.c.Invoker(), x.c.CodecFor(&x.cfg), &x.cfg, ", ref, ", in, &x.buf)")
		g.P(p...)
	case "ClientStream":
		p := []any{"return ", x.pkgs.pgrpc.Ident("NewClientStream"), "["}
		p = append(p, m.Input.GoIdent, ", ", m.Output.GoIdent, "](ctx, x.c.Invoker(), x.c.CodecFor(&x.cfg), &x.cfg, ", ref, ", 0)")
		g.P(p...)
	case "BidiStream":
		p := []any{"return ", x.pkgs.pgrpc.Ident("NewBidiStream"), "["}
		p = append(p, m.Input.GoIdent, ", ", m.Output.GoIdent, "](ctx, x.c.Invoker(), x.c.CodecFor(&x.cfg), &x.cfg, ", ref, ", 0)")
		g.P(p...)
	}
	g.P("}")
	g.P()
}
