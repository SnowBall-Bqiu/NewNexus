package api

// These acceptance tests run production REST handlers, durable jobs, control
// services, provider HTTP adapters, authoritative DNS wire verification and the
// agent gRPC protocol. Only third-party provider/DNS/webhook servers are local
// fixtures; production routes cannot inject these transports.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/agenthub"
	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/control"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/optimizer"
	"github.com/newnexus/newnexus/internal/provider"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"github.com/newnexus/newnexus/internal/testutil"
	"golang.org/x/net/dns/dnsmessage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

type acceptanceCloud struct {
	mu                         sync.Mutex
	rows                       map[string][]provider.Record
	next                       int
	hostname, fallback         string
	certificateReady           bool
	dnsQueries, signedRequests int
}

func (f *acceptanceCloud) snapshot(kind string) []provider.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.Record{}, f.rows[kind]...)
}
func (f *acceptanceCloud) host() domain.Doc {
	ownership, cert := false, false
	for _, r := range f.rows["cloudflare"] {
		if r.Type == "TXT" && r.Name == "_cf-custom-hostname."+f.hostname && r.Value == "ownership-token" {
			ownership = true
		}
		if r.Type == "TXT" && r.Name == "_acme-challenge."+f.hostname && r.Value == "certificate-token" {
			cert = true
		}
	}
	status, ssl := "pending", "pending_validation"
	if ownership {
		status = "active"
	}
	if cert && f.certificateReady {
		ssl = "active"
	}
	return domain.Doc{"id": "custom-host-id", "hostname": f.hostname, "status": status, "ownership_verification": domain.Doc{"name": "_cf-custom-hostname." + f.hostname, "value": "ownership-token", "type": "txt"}, "ssl": domain.Doc{"status": ssl, "validation_records": []domain.Doc{{"txt_name": "_acme-challenge." + f.hostname, "txt_value": "certificate-token"}}}}
}
func (f *acceptanceCloud) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	cf := func(v any) { reply(domain.Doc{"success": true, "result": v}) }
	path := r.URL.Path
	if strings.HasPrefix(path, "/cloudflare") {
		if r.Header.Get("Authorization") != "Bearer test-cloudflare-secret" {
			w.WriteHeader(401)
			cf(nil)
			return
		}
		path = strings.TrimPrefix(path, "/cloudflare")
		switch {
		case path == "/zones":
			cf([]domain.Doc{{"id": "cf-zone", "name": "example.com"}})
		case strings.HasSuffix(path, "/custom_hostnames/quota"):
			cf(domain.Doc{"allocated": 100, "used": 1, "exceeded": false, "hard_cap": 500, "scope": "zone"})
		case strings.HasSuffix(path, "/custom_hostnames/fallback_origin"):
			if r.Method == "PUT" {
				d := domain.Doc{}
				_ = json.NewDecoder(r.Body).Decode(&d)
				f.fallback = domain.S(d, "origin")
			}
			cf(domain.Doc{"origin": f.fallback, "status": "active"})
		case path == "/zones/cf-zone/custom_hostnames":
			if r.Method == "GET" {
				cf([]domain.Doc{})
				return
			}
			d := domain.Doc{}
			_ = json.NewDecoder(r.Body).Decode(&d)
			f.hostname = domain.S(d, "hostname")
			cf(f.host())
		case strings.HasSuffix(path, "/custom_hostnames/custom-host-id"):
			cf(f.host())
		case strings.HasSuffix(path, "/dns_records") && r.Method == "GET":
			rows := []domain.Doc{}
			for _, v := range f.rows["cloudflare"] {
				rows = append(rows, domain.Doc{"id": v.ID, "name": v.Name, "type": v.Type, "content": v.Value, "ttl": v.TTL, "proxied": v.Proxied})
			}
			cf(rows)
		case strings.Contains(path, "/dns_records"):
			id := path[strings.LastIndex(path, "/")+1:]
			if r.Method == "POST" {
				f.next++
				id = fmt.Sprintf("cf-record-%d", f.next)
			}
			kept := []provider.Record{}
			for _, v := range f.rows["cloudflare"] {
				if v.ID != id {
					kept = append(kept, v)
				}
			}
			if r.Method != "DELETE" {
				d := domain.Doc{}
				_ = json.NewDecoder(r.Body).Decode(&d)
				kept = append(kept, provider.Record{ID: id, Name: domain.S(d, "name"), Type: domain.S(d, "type"), Value: domain.S(d, "content"), TTL: int(domain.N(d, "ttl")), Line: "default", Weight: 1, Proxied: domain.B(d, "proxied")})
			}
			f.rows["cloudflare"] = kept
			cf(domain.Doc{"id": id})
		default:
			w.WriteHeader(404)
			reply(domain.Doc{"success": false, "errors": []domain.Doc{{"code": 1000}}})
		}
		return
	}
	if path == "/dnspod" {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "TC3-HMAC-SHA256 Credential=test-id/") {
			w.WriteHeader(401)
			return
		}
		f.signedRequests++
		d := domain.Doc{}
		_ = json.NewDecoder(r.Body).Decode(&d)
		respond := func(v domain.Doc) { reply(domain.Doc{"Response": v}) }
		switch r.Header.Get("X-TC-Action") {
		case "DescribeDomainList":
			respond(domain.Doc{"DomainList": []domain.Doc{{"Name": "example.com", "Grade": "DP_Free"}}})
		case "DescribeDomain":
			respond(domain.Doc{"DomainInfo": domain.Doc{"Grade": "DP_Free"}})
		case "DescribeRecordLineList":
			respond(domain.Doc{"LineList": []domain.Doc{{"LineId": "0", "Name": "默认"}, {"LineId": "telecom", "Name": "电信"}, {"LineId": "unicom", "Name": "联通"}}})
		case "DescribeDomainPurview":
			respond(domain.Doc{"PurviewList": []domain.Doc{{"Name": "最小TTL", "Value": 60}, {"Name": "负载均衡数量", "Value": 2}}})
		case "DescribeRecordList":
			items := []domain.Doc{}
			for _, v := range f.rows["dnspod"] {
				items = append(items, domain.Doc{"RecordId": v.ID, "Name": strings.TrimSuffix(v.Name, ".example.com"), "Type": v.Type, "Value": v.Value, "TTL": v.TTL, "LineId": v.Line, "Weight": v.Weight})
			}
			respond(domain.Doc{"RecordList": items})
		case "CreateRecord", "ModifyRecord", "DeleteRecord":
			id := domain.S(d, "RecordId")
			if r.Header.Get("X-TC-Action") == "CreateRecord" {
				f.next++
				id = fmt.Sprint(f.next + 1000)
			}
			kept := []provider.Record{}
			for _, v := range f.rows["dnspod"] {
				if v.ID != id {
					kept = append(kept, v)
				}
			}
			if r.Header.Get("X-TC-Action") != "DeleteRecord" {
				name := domain.S(d, "SubDomain") + ".example.com"
				line := domain.S(d, "RecordLineId")
				if line == "" {
					line = "default"
				}
				kept = append(kept, provider.Record{ID: id, Name: name, Type: domain.S(d, "RecordType"), Value: domain.S(d, "Value"), TTL: int(domain.N(d, "TTL")), Line: line, Weight: max(1, int(domain.N(d, "Weight")))})
			}
			f.rows["dnspod"] = kept
			respond(domain.Doc{"RecordId": id})
		default:
			w.WriteHeader(400)
			respond(domain.Doc{"Error": domain.Doc{"Code": "UnsupportedAction"}})
		}
		return
	}
	w.WriteHeader(404)
}

func (f *acceptanceCloud) authority(t *testing.T) string {
	t.Helper()
	server, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { server.Close() })
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, peer, e := server.ReadFrom(buffer)
			if e != nil {
				return
			}
			query := dnsmessage.Message{}
			if query.Unpack(buffer[:n]) != nil || len(query.Questions) != 1 {
				continue
			}
			q := query.Questions[0]
			response := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, Authoritative: true}, Questions: query.Questions}
			f.mu.Lock()
			f.dnsQueries++
			for _, rows := range f.rows {
				for _, r := range rows {
					if strings.TrimSuffix(q.Name.String(), ".") != r.Name {
						continue
					}
					header := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, Type: q.Type, TTL: uint32(r.TTL)}
					var body dnsmessage.ResourceBody
					switch {
					case r.Type == "TXT" && q.Type == dnsmessage.TypeTXT:
						body = &dnsmessage.TXTResource{TXT: []string{r.Value}}
					case r.Type == "CNAME" && q.Type == dnsmessage.TypeCNAME:
						name, _ := dnsmessage.NewName(r.Value + ".")
						body = &dnsmessage.CNAMEResource{CNAME: name}
					case r.Type == "A" && q.Type == dnsmessage.TypeA:
						ip := netip.MustParseAddr(r.Value)
						body = &dnsmessage.AResource{A: ip.As4()}
					case r.Type == "AAAA" && q.Type == dnsmessage.TypeAAAA:
						ip := netip.MustParseAddr(r.Value)
						body = &dnsmessage.AAAAResource{AAAA: ip.As16()}
					}
					if body != nil {
						response.Answers = append(response.Answers, dnsmessage.Resource{Header: header, Body: body})
					}
				}
			}
			f.mu.Unlock()
			raw, e := response.Pack()
			if e == nil {
				_, _ = server.WriteTo(raw, peer)
			}
		}
	}()
	return server.LocalAddr().String()
}

type acceptanceConsole struct {
	t      *testing.T
	s      *Server
	url    string
	client *http.Client
	cookie *http.Cookie
	csrf   string
}

func (c *acceptanceConsole) call(method, path string, payload domain.Doc, status int) domain.Doc {
	c.t.Helper()
	raw, _ := json.Marshal(payload)
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(raw)
	}
	r, e := http.NewRequest(method, c.url+"/api/v1/"+path, body)
	if e != nil {
		c.t.Fatal(e)
	}
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	r.Header.Set("X-CSRF-Token", c.csrf)
	r.Header.Set("Idempotency-Key", "acceptance:"+path+":"+domain.ID())
	r.Header.Set("Content-Type", "application/json")
	response, e := c.client.Do(r)
	if e != nil {
		c.t.Fatal(e)
	}
	defer response.Body.Close()
	d := domain.Doc{}
	_ = json.NewDecoder(response.Body).Decode(&d)
	if response.StatusCode != status {
		c.t.Fatalf("%s %s: expected %d got %d: %+v", method, path, status, response.StatusCode, d)
	}
	if path == "bootstrap" {
		c.cookie = response.Cookies()[0]
		c.csrf = domain.S(d, "csrf_token")
	}
	return d
}
func (c *acceptanceConsole) run(d domain.Doc) domain.Doc {
	c.t.Helper()
	jobID := domain.S(d, "job_id")
	if jobID == "" {
		c.t.Fatal("operation has no durable job", d)
	}
	job, e := c.s.Store.Get(context.Background(), "jobs", jobID)
	if e != nil {
		c.t.Fatal(e)
	}
	if e = c.s.runJob(context.Background(), job); e != nil {
		c.t.Fatal(e)
	}
	job = c.call("GET", "jobs/"+jobID, nil, 200)
	if domain.S(job, "status") != "succeeded" {
		c.t.Fatal("durable job did not succeed", job)
	}
	return job
}
func (c *acceptanceConsole) publish(kind string, d domain.Doc) domain.Doc {
	c.t.Helper()
	release := c.call("POST", kind+"/"+domain.S(d, "id")+"/preview", domain.Doc{}, 200)
	c.run(c.call("POST", "releases/"+domain.S(release, "id")+"/apply", domain.Doc{"confirmed": true}, 202))
	return c.call("GET", "releases/"+domain.S(release, "id"), nil, 200)
}

type acceptanceAgent struct {
	id     string
	stream agentproto.AgentService_ConnectClient
}

func (c *acceptanceConsole) agent(ctx context.Context, client agentproto.AgentServiceClient, name string) acceptanceAgent {
	c.t.Helper()
	token := c.call("POST", "nodes/enrollment-tokens", domain.Doc{"name": name}, 201)
	enroll, _ := structpb.NewStruct(map[string]any{"enrollment_token": token["token"], "name": name, "protocol_version": 1})
	response, e := client.Enroll(ctx, enroll)
	if e != nil {
		c.t.Fatal(e)
	}
	identity := domain.Doc(response.AsMap())
	stream, e := client.Connect(ctx)
	if e != nil {
		c.t.Fatal(e)
	}
	send := func(d domain.Doc) {
		m, _ := structpb.NewStruct(map[string]any(d))
		if e := stream.Send(m); e != nil {
			c.t.Fatal(e)
		}
	}
	send(domain.Doc{"type": "hello", "protocol_version": 1, "node_id": identity["node_id"], "credential": identity["credential"]})
	if _, e = stream.Recv(); e != nil {
		c.t.Fatal(e)
	}
	send(domain.Doc{"type": "heartbeat", "version": "acceptance-fixture", "capabilities": []any{"tcp", "http"}})
	return acceptanceAgent{id: domain.S(identity, "node_id"), stream: stream}
}
func (c *acceptanceConsole) sample(agent acceptanceAgent, candidate string, healthy bool) {
	c.t.Helper()
	ctx := context.Background()
	task := domain.Doc{"id": domain.ID(), "candidate_id": candidate, "address": "1.1.1.1", "mode": "all"}
	if e := c.s.Hub.Dispatch(ctx, agent.id, task); e != nil {
		c.t.Fatal(e)
	}
	for {
		received, e := agent.stream.Recv()
		if e != nil {
			c.t.Fatal(e)
		}
		d := domain.Doc(received.AsMap())
		if domain.S(d, "type") == "task" && domain.S(domain.M(d, "task"), "id") == domain.S(task, "id") {
			break
		}
	}
	rate := 0.
	if healthy {
		rate = 1
	}
	result := map[string]any{"id": agentproto.ResultID(agent.id, domain.S(task, "id")), "task_id": task["id"], "observed_at": domain.Now(), "success": healthy, "success_rate": rate, "sample_count": 3, "tcp_ms": 10., "tls_ms": 20., "ttfb_ms": 30., "download_bps": 1000000.}
	message, _ := structpb.NewStruct(map[string]any{"type": "results", "batch_id": domain.ID(), "results": []any{result}})
	if e := agent.stream.Send(message); e != nil {
		c.t.Fatal(e)
	}
	for {
		received, e := agent.stream.Recv()
		if e != nil {
			c.t.Fatal(e)
		}
		if domain.S(domain.Doc(received.AsMap()), "type") == "ack" {
			break
		}
	}
}

type acceptanceTransport func(*http.Request) (*http.Response, error)

func (f acceptanceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRESTAcceptanceWithProviderFixturesSaaSOptimizationRoutingAndSignedAlerts(t *testing.T) {
	t.Run("SQLite", func(t *testing.T) {
		db, err := store.Open(t.TempDir() + "/acceptance.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
		runRESTAcceptance(t, acceptanceServer(t, db))
	})
	t.Run("MySQL", func(t *testing.T) { runRESTAcceptance(t, acceptanceServer(t, testutil.MySQL(t))) })
	t.Run("PostgreSQL", func(t *testing.T) {
		dsn := os.Getenv("NEWNEXUS_TEST_POSTGRES")
		if dsn == "" {
			t.Skip("NEWNEXUS_TEST_POSTGRES is not configured")
		}
		admin, e := sql.Open("pgx", dsn)
		if e != nil {
			t.Fatal(e)
		}
		schema := "acceptance_" + strings.ReplaceAll(domain.ID(), "-", "")
		if _, e = admin.Exec(`CREATE SCHEMA ` + schema); e != nil {
			admin.Close()
			t.Fatal(e)
		}
		t.Cleanup(func() {
			_, e := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
			if e != nil {
				t.Error(e)
			}
			admin.Close()
		})
		u, e := url.Parse(dsn)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		db, e := store.Open(u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.DB.Close() })
		vault, e := security.NewVault(bytes.Repeat([]byte{1}, 32))
		if e != nil {
			t.Fatal(e)
		}
		s := New(db, vault, agenthub.New(db), rejectingOperations{})
		s.Auth.SecureCookie = false
		if e = s.EnsureDefaults(context.Background()); e != nil {
			t.Fatal(e)
		}
		runRESTAcceptance(t, s)
	})
}

func acceptanceServer(t *testing.T, db *store.Store) *Server {
	t.Helper()
	vault, err := security.NewVault(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := New(db, vault, agenthub.New(db), rejectingOperations{})
	s.Auth.SecureCookie = false
	if err = s.EnsureDefaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func runRESTAcceptance(t *testing.T, s *Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	fixture := &acceptanceCloud{rows: map[string][]provider.Record{"cloudflare": {{ID: "old-customer-address", Name: "customer.example.com", Type: "A", Value: "8.8.8.8", TTL: 300, Line: "default", Weight: 1}}}}
	external := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer external.Close()
	authority := fixture.authority(t)
	operations := control.New(s.Store, s.Vault)
	operations.Adapter = func(kind string, credentials domain.Doc) *provider.Adapter {
		a := provider.New(kind, credentials)
		a.BaseURL = external.URL + "/" + kind
		a.Client = external.Client()
		return a
	}
	operations.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	operations.Verify = func(ctx context.Context, zone string, records []provider.Record) error {
		return control.VerifyAuthoritativeServer(ctx, authority, zone, records)
	}
	s.Operations = operations
	consoleHTTP := httptest.NewServer(s)
	defer consoleHTTP.Close()
	console := &acceptanceConsole{t: t, s: s, url: consoleHTTP.URL, client: consoleHTTP.Client()}
	console.call("POST", "bootstrap", domain.Doc{"username": "admin", "password": "acceptance-admin-password"}, 200)
	project := console.call("POST", "projects", domain.Doc{"name": "端到端验收项目"}, 201)
	cloud := console.call("POST", "connections", domain.Doc{"name": "Cloudflare fixture", "provider": "cloudflare", "credentials": domain.Doc{"api_token": "test-cloudflare-secret"}}, 201)
	console.run(console.call("POST", "connections/"+domain.S(cloud, "id")+"/validate", domain.Doc{}, 202))
	pod := console.call("POST", "connections", domain.Doc{"name": "DNSPod fixture", "provider": "dnspod", "credentials": domain.Doc{"secret_id": "test-id", "secret_key": "test-key"}}, 201)
	console.run(console.call("POST", "connections/"+domain.S(pod, "id")+"/validate", domain.Doc{}, 202))
	zone := console.call("POST", "saas/zones", domain.Doc{"project_id": project["id"], "connection_id": cloud["id"], "zone_id": "cf-zone", "zone_name": "example.com", "fallback_origin": "optimized.example.com", "cname_target": "edge.example.net"}, 201)
	console.run(zone)
	h := console.call("POST", "saas/hostnames", domain.Doc{"saas_zone_id": zone["id"], "hostname": "customer.example.com", "validation_method": "txt", "automation_mode": "auto", "dns_connection_id": cloud["id"], "dns_zone_id": "cf-zone", "dns_zone_name": "example.com"}, 201)
	console.run(h)
	rows := fixture.snapshot("cloudflare")
	txt := 0
	for _, r := range rows {
		if r.Type == "TXT" {
			txt++
		}
		if r.Name == "customer.example.com" && r.Type == "CNAME" {
			t.Fatal("SaaS traffic cut over before dual active")
		}
	}
	if txt != 2 {
		t.Fatal("ownership and certificate TXT instructions were not both materialized", rows)
	}
	console.run(console.call("POST", "saas/hostnames/"+domain.S(h, "id")+"/sync", domain.Doc{}, 202))
	h = console.call("GET", "saas/hostnames/"+domain.S(h, "id"), nil, 200)
	if domain.S(h, "status") != "active" || domain.S(h, "ssl_status") == "active" {
		t.Fatal("SaaS ownership and SSL were not independent", h)
	}
	console.call("PUT", "saas/hostnames/"+domain.S(h, "id"), domain.Doc{"automation_mode": "manual"}, 200)
	fixture.mu.Lock()
	fixture.certificateReady = true
	fixture.mu.Unlock()
	console.run(console.call("POST", "saas/hostnames/"+domain.S(h, "id")+"/sync", domain.Doc{}, 202))
	migration := console.publish("saas/hostnames", h)
	if !domain.B(migration, "authoritative_verified") {
		t.Fatal("migration skipped authoritative DNS", migration)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	grpcServer := grpc.NewServer()
	agentproto.RegisterAgentServiceServer(grpcServer, s.Hub)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	connection, e := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		t.Fatal(e)
	}
	defer connection.Close()
	agentClient := agentproto.NewAgentServiceClient(connection)
	agents := []acceptanceAgent{}
	for i := 0; i < 6; i++ {
		agents = append(agents, console.agent(ctx, agentClient, fmt.Sprintf("fixture-node-%d", i)))
	}
	ipv4 := console.call("POST", "candidates", domain.Doc{"project_id": project["id"], "address": "1.1.1.1"}, 201)
	ipv6 := console.call("POST", "candidates", domain.Doc{"project_id": project["id"], "address": "2606:4700:4700::1111"}, 201)
	console.sample(agents[0], domain.S(ipv4, "id"), true)
	console.sample(agents[0], domain.S(ipv6, "id"), true)
	if _, e = optimizer.RefreshPending(ctx, s.Store, 100); e != nil {
		t.Fatal(e)
	}
	optimized := console.call("POST", "optimized-hostnames", domain.Doc{"project_id": project["id"], "hostname": "optimized.example.com", "dns_connection_id": cloud["id"], "zone_id": "cf-zone", "zone_name": "example.com", "family": 4, "top_n": 1, "ttl": 300}, 201)
	console.publish("optimized-hostnames", optimized)
	optimized = console.call("PUT", "optimized-hostnames/"+domain.S(optimized, "id"), domain.Doc{"family": 6}, 200)
	console.publish("optimized-hostnames", optimized)
	for _, r := range fixture.snapshot("cloudflare") {
		if r.Name == "optimized.example.com" && r.Type != "AAAA" {
			t.Fatal("IP family switch left stale A data", r)
		}
	}
	telecomIDs, unicomIDs := []any{}, []any{}
	for i, a := range agents {
		if i < 3 {
			telecomIDs = append(telecomIDs, a.id)
		} else {
			unicomIDs = append(unicomIDs, a.id)
		}
	}
	telecom := console.call("POST", "node-groups", domain.Doc{"name": "电信探测组", "node_ids": telecomIDs}, 201)
	unicom := console.call("POST", "node-groups", domain.Doc{"name": "联通探测组", "node_ids": unicomIDs}, 201)
	console.call("PUT", "settings", domain.Doc{"routing.failure_rounds": 1, "routing.recovery_rounds": 1, "routing.cooldown_seconds": 0}, 200)
	line := func(code string, group any, primary, backup string) domain.Doc {
		return domain.Doc{"provider_line_code": code, "node_group": group, "health_perspective": "node_group", "min_nodes": 3, "quorum_percent": 67, "targets": []domain.Doc{{"name": "主线", "cname": primary, "priority": 0, "enabled": true, "weight": 1}, {"name": "备用", "cname": backup, "priority": 1, "enabled": true, "weight": 1}}}
	}
	rule := console.call("POST", "rules", domain.Doc{"project_id": project["id"], "name": "多运营商线路", "hostname": "routed.example.com", "dns_connection_id": pod["id"], "zone_id": "example.com", "zone_name": "example.com", "ttl": 300, "switch_mode": "availability", "publish_mode": "manual", "lines": []domain.Doc{line("telecom", telecom["id"], "customer.example.com", "telecom-backup.example.net"), line("unicom", unicom["id"], "unicom.example.net", "unicom-backup.example.net")}}, 201)
	probeRound := func(telecomPrimary, allHealthy bool) {
		t.Helper()
		for li, l := range domain.A(rule, "lines") {
			for ti, target := range domain.A(l, "targets") {
				healthy := allHealthy
				if li == 0 && ti == 0 {
					healthy = telecomPrimary && allHealthy
				}
				start := li * 3
				for _, agent := range agents[start : start+3] {
					console.sample(agent, "target:"+domain.S(target, "id"), healthy)
				}
			}
		}
		console.call("POST", "rules/"+domain.S(rule, "id")+"/evaluate", domain.Doc{}, 200)
		rule = console.call("GET", "rules/"+domain.S(rule, "id"), nil, 200)
	}
	probeRound(true, true)
	console.publish("rules", rule)
	initial := fixture.snapshot("dnspod")
	if len(initial) != 2 {
		t.Fatal("provider line publication lost records", initial)
	}
	probeRound(false, true)
	lines := domain.A(rule, "lines")
	if domain.N(lines[0], "active_priority") != 1 || domain.N(lines[1], "active_priority") != 0 {
		t.Fatal("one line failure changed another line", rule)
	}
	release := console.call("POST", "rules/"+domain.S(rule, "id")+"/preview", domain.Doc{}, 200)
	diff := domain.A(release, "diff")
	if len(diff) != 1 || domain.S(diff[0], "line") != "telecom" {
		t.Fatal("preview changed an unaffected ISP line", release)
	}
	console.run(console.call("POST", "releases/"+domain.S(release, "id")+"/apply", domain.Doc{"confirmed": true}, 202))
	switched := fixture.snapshot("dnspod")
	probeRound(false, false)
	if domain.S(rule, "status") != "fail_stale" {
		t.Fatal("all-failure did not preserve stale DNS mode", rule)
	}
	console.call("POST", "rules/"+domain.S(rule, "id")+"/preview", domain.Doc{}, 400)
	if fmt.Sprint(fixture.snapshot("dnspod")) != fmt.Sprint(switched) {
		t.Fatal("all-failure changed live DNS")
	}
	console.run(console.call("POST", "releases/"+domain.S(release, "id")+"/rollback", domain.Doc{"confirmed": true}, 202))
	restored := fixture.snapshot("dnspod")
	for _, old := range initial {
		found := false
		for _, r := range restored {
			if r.Line == old.Line && r.Value == old.Value {
				found = true
			}
		}
		if !found {
			t.Fatal("rollback failed to restore ISP snapshot", initial, restored)
		}
	}
	// Automatic mode uses the same real control preview/apply boundary: a
	// small change gets an apply job, a large change requires review, and
	// fail-stale never queues another publication or changes external DNS.
	probeRound(true, true)
	rule = console.call("PUT", "rules/"+domain.S(rule, "id"), domain.Doc{"publish_mode": "auto"}, 200)
	applyJobs := func() map[string]bool {
		out := map[string]bool{}
		jobs, _ := s.Store.List(ctx, "jobs")
		for _, j := range jobs {
			if domain.S(j, "type") == "apply" {
				out[domain.S(j, "id")] = true
			}
		}
		return out
	}
	beforeJobs := applyJobs()
	probeRound(false, true)
	jobs, _ := s.Store.List(ctx, "jobs")
	small := 0
	for _, j := range jobs {
		if domain.S(j, "type") == "apply" && !beforeJobs[domain.S(j, "id")] {
			release, _ := s.Store.Get(ctx, "releases", domain.S(j, "resource_id"))
			if len(domain.A(release, "diff")) != 1 {
				t.Fatal("small automatic change had unexpected scope", release)
			}
			console.run(domain.Doc{"job_id": j["id"]})
			small++
		}
	}
	if small != 1 {
		t.Fatal("quorum did not enqueue exactly one small automatic publication", small)
	}
	beforeJobs = applyJobs()
	probeRound(true, true)
	jobs, _ = s.Store.List(ctx, "jobs")
	for _, j := range jobs {
		if domain.S(j, "type") == "apply" && !beforeJobs[domain.S(j, "id")] {
			console.run(domain.Doc{"job_id": j["id"]})
		}
	}
	console.call("PUT", "settings", domain.Doc{"publish.large_change_threshold": 1}, 200)
	beforeJobs = applyJobs()
	beforeReleases := s.Store.Count(ctx, "releases")
	dnsBeforeLarge := fmt.Sprint(fixture.snapshot("dnspod"))
	sampleAll := func(allFailed bool) {
		for li, l := range domain.A(rule, "lines") {
			for ti, target := range domain.A(l, "targets") {
				for _, a := range agents[li*3 : li*3+3] {
					console.sample(a, "target:"+domain.S(target, "id"), !allFailed && ti != 0)
				}
			}
		}
		console.call("POST", "rules/"+domain.S(rule, "id")+"/evaluate", domain.Doc{}, 200)
		rule = console.call("GET", "rules/"+domain.S(rule, "id"), nil, 200)
	}
	sampleAll(false)
	afterReleases := s.Store.Count(ctx, "releases")
	if afterReleases != beforeReleases+1 || len(applyJobs()) != len(beforeJobs) {
		t.Fatal("large automatic change was not left for explicit confirmation", beforeReleases, afterReleases, len(beforeJobs), len(applyJobs()))
	}
	if fmt.Sprint(fixture.snapshot("dnspod")) != dnsBeforeLarge {
		t.Fatal("large change wrote DNS without confirmation")
	}
	beforeReleases = afterReleases
	sampleAll(true)
	afterReleases = s.Store.Count(ctx, "releases")
	if domain.S(rule, "status") != "fail_stale" || afterReleases != beforeReleases || len(applyJobs()) != len(beforeJobs) || fmt.Sprint(fixture.snapshot("dnspod")) != dnsBeforeLarge {
		t.Fatal("all failure queued or performed automatic DNS publication", rule)
	}
	secret := "acceptance-hmac-secret-value"
	deliveries := 0
	webhook := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(payload)
		expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-NewNexus-Signature"))) {
			t.Error("webhook signature mismatch")
		}
		if r.Header.Get("X-NewNexus-Event-ID") == "" {
			t.Error("webhook has no event id")
		}
		deliveries++
		w.WriteHeader(204)
	}))
	defer webhook.Close()
	transport := webhook.Client().Transport
	s.webhookClient = &http.Client{Transport: acceptanceTransport(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		fixtureURL, _ := http.NewRequest("POST", webhook.URL, nil)
		u.Scheme = fixtureURL.URL.Scheme
		u.Host = fixtureURL.URL.Host
		copy.URL = &u
		copy.Host = u.Host
		return transport.RoundTrip(copy)
	}), Timeout: 5 * time.Second}
	console.call("PUT", "settings", domain.Doc{"alerts.webhook_url": "https://hooks.example.com/event", "alerts.webhook_secret": secret}, 200)
	s.monitorExternal(context.Background(), time.Now())
	jobs, _ = s.Store.List(context.Background(), "jobs")
	for _, job := range jobs {
		if domain.S(job, "resource_type") == "alerts" && domain.S(job, "type") == "notify" && domain.S(job, "status") == "queued" {
			console.run(domain.Doc{"job_id": job["id"]})
		}
	}
	if deliveries == 0 {
		t.Fatal("routing incidents never reached a signed webhook")
	}
	fixture.mu.Lock()
	dnsQueries, signedRequests := fixture.dnsQueries, fixture.signedRequests
	fixture.mu.Unlock()
	if dnsQueries < 10 || signedRequests == 0 {
		t.Fatal("acceptance did not traverse real DNS/provider transports", dnsQueries, signedRequests)
	}
	history := console.call("GET", "metrics/history?resource_type=rules&id="+domain.S(rule, "id")+"&line_id="+domain.S(domain.A(rule, "lines")[0], "id")+"&resolution=five_minute&hours=24", nil, 200)
	if len(domain.A(history, "items")) == 0 {
		t.Fatal("agent ingestion did not reach real metric rollups")
	}
	audit := console.call("GET", "audit?limit=500", nil, 200)
	if domain.N(audit, "total") < 10 {
		t.Fatal("REST operations were not audited")
	}
}
