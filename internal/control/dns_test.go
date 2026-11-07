package control

import (
	"context"
	"net"
	"testing"

	"github.com/newnexus/newnexus/internal/provider"
	"golang.org/x/net/dns/dnsmessage"
)

func dnsServer(t *testing.T, authoritative bool, value string) string {
	t.Helper()
	connection, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { connection.Close() })
	go func() {
		packet := make([]byte, 4096)
		for {
			n, remote, e := connection.ReadFrom(packet)
			if e != nil {
				return
			}
			request := dnsmessage.Message{}
			if e = request.Unpack(packet[:n]); e != nil {
				continue
			}
			response := dnsmessage.Message{Header: dnsmessage.Header{ID: request.ID, Response: true, Authoritative: authoritative}, Questions: request.Questions}
			if len(request.Questions) > 0 && value != "" {
				question := request.Questions[0]
				name, _ := dnsmessage.NewName(value + ".")
				response.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 300}, Body: &dnsmessage.CNAMEResource{CNAME: name}}}
			}
			raw, e := response.Pack()
			if e == nil {
				_, _ = connection.WriteTo(raw, remote)
			}
		}
	}()
	return connection.LocalAddr().String()
}

func TestAuthoritativeVerificationRejectsRecursiveAndUnexpectedAnswers(t *testing.T) {
	desired := []provider.Record{{Name: "www.example.com", Type: "CNAME", Value: "edge.example.net", Line: "default", Weight: 1}, {Name: "www.example.com", Type: "CNAME", Value: "backup.example.net", Line: "telecom", Weight: 20}}
	if e := verifyAt(context.Background(), dnsServer(t, false, "edge.example.net"), "example.com", desired); e == nil {
		t.Fatal("recursive answers must not pass authoritative validation")
	}
	if e := verifyAt(context.Background(), dnsServer(t, true, "external.example.net"), "example.com", desired); e == nil {
		t.Fatal("unreviewed target must not pass authoritative validation")
	}
	if e := verifyAt(context.Background(), dnsServer(t, true, "backup.example.net"), "example.com", desired); e != nil {
		t.Fatalf("a valid geographic/weighted subset was rejected: %v", e)
	}
}

func TestAuthoritativeVerificationChecksDeletionAsAbsence(t *testing.T) {
	tombstones := verificationSet(nil, []provider.Record{{Name: "www.example.com", Type: "CNAME", Value: "old.example.net"}})
	if len(tombstones) != 1 || tombstones[0].Value != "" {
		t.Fatal("deletion check did not preserve owner/type")
	}
	if e := verifyAt(context.Background(), dnsServer(t, true, "old.example.net"), "example.com", tombstones); e == nil {
		t.Fatal("a lingering deleted record passed verification")
	}
	if e := verifyAt(context.Background(), dnsServer(t, true, ""), "example.com", tombstones); e != nil {
		t.Fatalf("authoritative absence must pass: %v", e)
	}
}
