package nicoagent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Asset holds NICo's Forge API definition (packs/nvidia/pack.yaml). Neither the files nor code
// generated from them are in this repository: the adapter compiles them when it starts, as the
// nmxc adapter compiles NMX-C's (D13).
const Asset = "nico-forge-proto"

// ProtoFile is the Forge service's file within the asset; it imports the rest.
const ProtoFile = "forge.proto"

// ServiceName is the gRPC service nico-api serves.
const ServiceName = "forge.Forge"

// Forge is the compiled Forge API: its service and a resolver for its messages.
type Forge struct {
	Service protoreflect.ServiceDescriptor
	Files   *protoregistry.Files
}

// LoadForge compiles the obtained definition, or says how to obtain it.
func LoadForge(reg *pack.Registry) (*Forge, error) {
	path, err := twin.AssetPath(reg, Asset+"/"+ProtoFile)
	if err != nil {
		return nil, fmt.Errorf("NICo's Forge API definition is not available: %w", err)
	}
	return CompileForge(filepath.Dir(path))
}

// CompileForge compiles forge.proto and its imports from a directory.
func CompileForge(dir string) (*Forge, error) {
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{dir}})}
	files, err := c.Compile(context.Background(), ProtoFile)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", filepath.Join(dir, ProtoFile), err)
	}
	reg := new(protoregistry.Files)
	var visit func(fd protoreflect.FileDescriptor) error
	visit = func(fd protoreflect.FileDescriptor) error {
		if _, err := reg.FindFileByPath(fd.Path()); err == nil {
			return nil
		}
		imps := fd.Imports()
		for i := 0; i < imps.Len(); i++ {
			if err := visit(imps.Get(i).FileDescriptor); err != nil {
				return err
			}
		}
		return reg.RegisterFile(fd)
	}
	if err := visit(files[0]); err != nil {
		return nil, err
	}
	sd := files[0].Services().ByName("Forge")
	if sd == nil {
		return nil, fmt.Errorf("%s defines no %s service", ProtoFile, ServiceName)
	}
	return &Forge{Service: sd, Files: reg}, nil
}

// obj is a message written as protojson (field names in lowerCamel or as in the .proto, enum
// values by name); call builds the request from it and returns the response the same way.
type obj = map[string]any

// call invokes a unary method of the Forge service on conn.
func (f *Forge) call(ctx context.Context, conn grpc.ClientConnInterface, method string, req obj) (obj, error) {
	md := f.Service.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		return nil, fmt.Errorf("the Forge service has no method %s", method)
	}
	in := dynamicpb.NewMessage(md.Input())
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := (protojson.UnmarshalOptions{Resolver: f.types()}).Unmarshal(b, in); err != nil {
		return nil, fmt.Errorf("%s request: %w", method, err)
	}
	out := dynamicpb.NewMessage(md.Output())
	if err := conn.Invoke(ctx, fmt.Sprintf("/%s/%s", f.Service.FullName(), md.Name()), in, out); err != nil {
		return nil, err
	}
	ob, err := (protojson.MarshalOptions{Resolver: f.types(), UseProtoNames: true}).Marshal(out)
	if err != nil {
		return nil, err
	}
	var res obj
	if err := json.Unmarshal(ob, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// types resolves the messages the Forge API names (Any and its well-known types).
func (f *Forge) types() *dynamicTypes { return &dynamicTypes{f.Files} }

type dynamicTypes struct{ files *protoregistry.Files }

func (t *dynamicTypes) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	if mt, err := protoregistry.GlobalTypes.FindMessageByName(name); err == nil {
		return mt, nil
	}
	d, err := t.files.FindDescriptorByName(name)
	if err != nil {
		return nil, err
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("%s is not a message", name)
	}
	return dynamicpb.NewMessageType(md), nil
}

func (t *dynamicTypes) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	name := url
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '/' {
			name = url[i+1:]
			break
		}
	}
	return t.FindMessageByName(protoreflect.FullName(name))
}

func (t *dynamicTypes) FindExtensionByName(name protoreflect.FullName) (protoreflect.ExtensionType, error) {
	return protoregistry.GlobalTypes.FindExtensionByName(name)
}

func (t *dynamicTypes) FindExtensionByNumber(m protoreflect.FullName, n protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	return protoregistry.GlobalTypes.FindExtensionByNumber(m, n)
}
