package nmxc

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Asset is the obtained asset that holds NMX-C's interface definition (packs/nvidia/pack.yaml).
// Its licence is NVIDIA's proprietary one, so neither the file nor code generated from it is in
// this repository (D13): the adapter compiles it when it starts.
const Asset = "nico-nmxc-proto"

// ProtoFile is the interface definition's file name within the asset.
const ProtoFile = "nmx_c.proto"

// ServiceName is the gRPC service the adapter serves.
const ServiceName = "nmx_c.NMX_Controller"

// Protoset is the compiled interface definition: the file, its service, and a registry that
// resolves its messages (for server reflection and for clients that build requests from it).
type Protoset struct {
	File    protoreflect.FileDescriptor
	Service protoreflect.ServiceDescriptor
	Files   *protoregistry.Files
}

// LoadProto compiles the obtained interface definition, or says how to obtain it.
func LoadProto(reg *pack.Registry) (*Protoset, error) {
	path, err := twin.AssetPath(reg, Asset+"/"+ProtoFile)
	if err != nil {
		return nil, fmt.Errorf("NMX-C's interface definition is not available: %w", err)
	}
	return CompileProto(filepath.Dir(path))
}

// CompileProto compiles nmx_c.proto from a directory into descriptors.
func CompileProto(dir string) (*Protoset, error) {
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{dir}})}
	files, err := c.Compile(context.Background(), ProtoFile)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", filepath.Join(dir, ProtoFile), err)
	}
	fd := files[0]
	sd := fd.Services().ByName(protoreflect.Name(ServiceName[len("nmx_c."):]))
	if sd == nil || string(sd.FullName()) != ServiceName {
		return nil, fmt.Errorf("%s defines no %s service", ProtoFile, ServiceName)
	}
	reg := new(protoregistry.Files)
	if err := reg.RegisterFile(fd); err != nil {
		return nil, err
	}
	return &Protoset{File: fd, Service: sd, Files: reg}, nil
}

// Message returns a message descriptor by its name in the nmx_c package.
func (p *Protoset) Message(name string) protoreflect.MessageDescriptor {
	return p.File.Messages().ByName(protoreflect.Name(name))
}

// Method returns a method of the service.
func (p *Protoset) Method(name string) protoreflect.MethodDescriptor {
	return p.Service.Methods().ByName(protoreflect.Name(name))
}

// obj is a message written as its proto field names, enum values by name; Build turns it into
// a message of the compiled definition.
type obj = map[string]any

// Build makes a message of a descriptor from fields named as the proto names them. A name that
// is not a field is an error, so a projection cannot silently drop a value.
func Build(md protoreflect.MessageDescriptor, v obj) (*dynamicpb.Message, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := dynamicpb.NewMessage(md)
	if err := protojson.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s: %w", md.FullName(), err)
	}
	return m, nil
}

// Decode reads a message into a Go value through its JSON form with proto field names.
func Decode(m proto.Message, into any) error {
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// u64 is a 64-bit unsigned field as protojson writes it (a string) or as a number.
type u64 uint64

func (v *u64) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n uint64
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*v = u64(n)
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	*v = u64(n)
	return err
}

// enum is an enum field: its value name, or its number when the client sent a value the
// definition does not name.
type enum string

func (v *enum) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*v = enum(s)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*v = enum(strconv.FormatInt(n, 10))
	return nil
}

// is reports whether the enum holds a value, treating an unset field and a *_UNDEFINED/_UNKNOWN
// zero as unset.
func (v enum) set() bool {
	return v != "" && v != "0" && !hasSuffix(string(v), "_UNDEFINED") && !hasSuffix(string(v), "_UNKNOWN")
}

func hasSuffix(s, suf string) bool { return len(s) >= len(suf) && s[len(s)-len(suf):] == suf }
