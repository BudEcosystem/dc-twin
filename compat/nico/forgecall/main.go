// Command forgecall calls one method of NICo's Forge gRPC API with a JSON request and prints the
// JSON response: what an operator's tooling does where NICo's admin CLI has no command (creating a
// tenant). The API is compiled from NICo's own forge.proto (the twin's nico-forge-proto asset), as
// the twin's nicoagent adapter compiles it.
//
//	forgecall -proto <dir with forge.proto> -addr localhost:11079 -ca ca.crt -cert client.crt -key client.key \
//	  CreateTenant '{"organization_id":"tenant-a","metadata":{"name":"tenant-a"}}'
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/BudEcosystem/dc-twin/internal/adapter/nicoagent"
)

func main() {
	protoDir := flag.String("proto", "", "the directory holding NICo's forge.proto and its imports")
	addr := flag.String("addr", "localhost:11079", "nico-api's address")
	ca := flag.String("ca", "", "the site's root CA (PEM)")
	cert := flag.String("cert", "", "a client certificate (PEM)")
	key := flag.String("key", "", "its key (PEM)")
	flag.Parse()
	if flag.NArg() != 2 || *protoDir == "" {
		fmt.Fprintln(os.Stderr, "usage: forgecall -proto <dir> -addr <host:port> -ca <ca> -cert <crt> -key <key> <Method> '<json request>'")
		os.Exit(2)
	}
	if err := run(*protoDir, *addr, *ca, *cert, *key, flag.Arg(0), flag.Arg(1)); err != nil {
		fmt.Fprintln(os.Stderr, "forgecall:", err)
		os.Exit(1)
	}
}

func run(protoDir, addr, ca, cert, key, method, req string) error {
	f, err := nicoagent.CompileForge(protoDir)
	if err != nil {
		return err
	}
	md := f.Service.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		return fmt.Errorf("%s has no method %s", nicoagent.ServiceName, method)
	}
	in := dynamicpb.NewMessage(md.Input())
	if err := (protojson.UnmarshalOptions{}).Unmarshal([]byte(req), in); err != nil {
		return fmt.Errorf("the request: %w", err)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != "" {
		b, err := os.ReadFile(ca)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(b)
		cfg.RootCAs = pool
	}
	if cert != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return err
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out := dynamicpb.NewMessage(md.Output())
	if err := conn.Invoke(ctx, "/"+nicoagent.ServiceName+"/"+string(md.Name()), in, out); err != nil {
		return err
	}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(out)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
