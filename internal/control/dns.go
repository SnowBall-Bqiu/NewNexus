package control

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/newnexus/newnexus/internal/provider"
	"github.com/newnexus/newnexus/internal/security"
	"golang.org/x/net/dns/dnsmessage"
)

// VerifyAuthoritative queries the zone's real authoritative servers directly,
// with RD=0 and AA required. A weighted/geographic answer may contain a subset
// of the reviewed values; complete line configuration is checked via the API.
func VerifyAuthoritative(parent context.Context, zone string, want []provider.Record) error {
	if len(want) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	servers, e := net.DefaultResolver.LookupNS(ctx, zone)
	if e != nil || len(servers) == 0 {
		return errors.New("cannot discover authoritative nameservers")
	}
	addresses := []string{}
	for _, server := range servers {
		ips, err := security.ResolvePublic(ctx, canonical(server.Host))
		if err != nil {
			return fmt.Errorf("cannot resolve authoritative nameserver: %w", err)
		}
		for _, ip := range ips {
			addresses = append(addresses, net.JoinHostPort(ip.String(), "53"))
			break
		}
	}
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		last = nil
		for _, address := range addresses {
			if e = verifyAt(ctx, address, zone, want); e != nil {
				last = e
				break
			}
		}
		if last == nil {
			return nil
		}
		if attempt < 3 {
			timer := time.NewTimer(time.Duration(attempt+1) * 2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return last
}

func verifyAt(ctx context.Context, address, zone string, want []provider.Record) error {
	groups := map[string][]provider.Record{}
	for _, r := range want {
		key := canonical(r.Name) + "|" + r.Type
		groups[key] = append(groups[key], r)
	}
	for _, group := range groups {
		first := group[0]
		typ, e := dnsType(first.Type)
		if e != nil {
			return e
		}
		values, e := queryDNS(ctx, address, first.Name, typ)
		if e != nil {
			return e
		}
		allowed := map[string]bool{}
		absent := true
		for _, r := range group {
			if r.Value != "" {
				absent = false
				value := r.Value
				if r.Type == "CNAME" {
					value = canonical(value)
				}
				allowed[value] = true
			}
		}
		if absent {
			if len(values) > 0 {
				return fmt.Errorf("authoritative %s %s still exists", first.Name, first.Type)
			}
			continue
		}
		if first.Type == "CNAME" && canonical(first.Name) == canonical(zone) && len(values) == 0 {
			// Cloudflare's apex CNAME flattening is visible as A/AAAA, not CNAME.
			resolved := map[string]bool{}
			for _, r := range group {
				ips, err := security.ResolvePublic(ctx, r.Value)
				if err != nil {
					return err
				}
				for _, ip := range ips {
					resolved[ip.String()] = true
				}
			}
			count := 0
			for _, ipType := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
				answers, err := queryDNS(ctx, address, first.Name, ipType)
				if err != nil {
					return err
				}
				for _, value := range answers {
					if !resolved[value] {
						return errors.New("flattened apex returned an unexpected IP address")
					}
					count++
				}
			}
			if count == 0 {
				return errors.New("flattened apex has no authoritative addresses")
			}
			continue
		}
		if len(values) == 0 {
			return fmt.Errorf("authoritative %s %s has no answer", first.Name, first.Type)
		}
		for _, value := range values {
			if first.Type == "CNAME" {
				value = canonical(value)
			}
			if !allowed[value] {
				return fmt.Errorf("authoritative %s %s differs from the reviewed values", first.Name, first.Type)
			}
		}
	}
	return nil
}

// VerifyAuthoritativeServer validates one authority using the same DNS wire
// checks as discovery-based verification. The caller supplies a vetted server
// address; the REST API never accepts an override for the production resolver.
func VerifyAuthoritativeServer(ctx context.Context, address, zone string, want []provider.Record) error {
	return verifyAt(ctx, address, zone, want)
}

func dnsType(name string) (dnsmessage.Type, error) {
	switch name {
	case "A":
		return dnsmessage.TypeA, nil
	case "AAAA":
		return dnsmessage.TypeAAAA, nil
	case "CNAME":
		return dnsmessage.TypeCNAME, nil
	case "TXT":
		return dnsmessage.TypeTXT, nil
	}
	return 0, errors.New("unsupported DNS verification record type")
}

func queryDNS(ctx context.Context, address, host string, typ dnsmessage.Type) ([]string, error) {
	name, e := dnsmessage.NewName(canonical(host) + ".")
	if e != nil {
		return nil, e
	}
	idBytes := [2]byte{}
	if _, e = rand.Read(idBytes[:]); e != nil {
		return nil, e
	}
	id := binary.BigEndian.Uint16(idBytes[:])
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: id}, Questions: []dnsmessage.Question{{Name: name, Type: typ, Class: dnsmessage.ClassINET}}}
	packet, e := query.Pack()
	if e != nil {
		return nil, e
	}
	response, e := dnsExchange(ctx, address, packet, false)
	if e != nil {
		return nil, e
	}
	message := dnsmessage.Message{}
	if e = message.Unpack(response); e != nil {
		return nil, e
	}
	if message.Truncated {
		response, e = dnsExchange(ctx, address, packet, true)
		if e != nil {
			return nil, e
		}
		if e = message.Unpack(response); e != nil {
			return nil, e
		}
	}
	if !message.Response || message.ID != id || !message.Authoritative {
		return nil, errors.New("DNS response is not an authoritative answer for this query")
	}
	if message.RCode != dnsmessage.RCodeSuccess && message.RCode != dnsmessage.RCodeNameError {
		return nil, fmt.Errorf("authoritative DNS returned %s", message.RCode)
	}
	if len(message.Questions) != 1 || message.Questions[0].Name != name || message.Questions[0].Type != typ {
		return nil, errors.New("DNS response question mismatch")
	}
	out := []string{}
	for _, answer := range message.Answers {
		if canonical(answer.Header.Name.String()) != canonical(host) {
			continue
		}
		switch body := answer.Body.(type) {
		case *dnsmessage.AResource:
			if typ == dnsmessage.TypeA {
				out = append(out, netip.AddrFrom4(body.A).String())
			}
		case *dnsmessage.AAAAResource:
			if typ == dnsmessage.TypeAAAA {
				out = append(out, netip.AddrFrom16(body.AAAA).String())
			}
		case *dnsmessage.CNAMEResource:
			if typ == dnsmessage.TypeCNAME {
				out = append(out, canonical(body.CNAME.String()))
			}
		case *dnsmessage.TXTResource:
			if typ == dnsmessage.TypeTXT {
				out = append(out, strings.Join(body.TXT, ""))
			}
		}
	}
	return out, nil
}

func dnsExchange(ctx context.Context, address string, packet []byte, tcp bool) ([]byte, error) {
	network := "udp"
	if tcp {
		network = "tcp"
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	connection, e := dialer.DialContext(ctx, network, address)
	if e != nil {
		return nil, e
	}
	defer connection.Close()
	deadline := time.Now().Add(3 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if e = connection.SetDeadline(deadline); e != nil {
		return nil, e
	}
	if tcp {
		header := [2]byte{}
		binary.BigEndian.PutUint16(header[:], uint16(len(packet)))
		if _, e = connection.Write(append(header[:], packet...)); e != nil {
			return nil, e
		}
		if _, e = io.ReadFull(connection, header[:]); e != nil {
			return nil, e
		}
		length := binary.BigEndian.Uint16(header[:])
		response := make([]byte, length)
		_, e = io.ReadFull(connection, response)
		return response, e
	}
	if _, e = connection.Write(packet); e != nil {
		return nil, e
	}
	response := make([]byte, 65535)
	n, e := connection.Read(response)
	return response[:n], e
}

func verificationSet(after, before []provider.Record) []provider.Record {
	out := append([]provider.Record{}, after...)
	groups := map[string]bool{}
	for _, r := range after {
		groups[canonical(r.Name)+"|"+r.Type] = true
	}
	for _, r := range before {
		key := canonical(r.Name) + "|" + r.Type
		if !groups[key] {
			r.Value = ""
			out = append(out, r)
			groups[key] = true
		}
	}
	return out
}
