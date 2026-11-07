// Command loadtest exercises the real database repository, scheduler, routing
// evaluation and streaming gRPC result path. Probe observations are synthetic;
// this tool never contacts a CDN or a DNS provider and never uses production data.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/newnexus/newnexus/internal/agentclient"
	"github.com/newnexus/newnexus/internal/agenthub"
	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/api"
	"github.com/newnexus/newnexus/internal/control"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

type config struct {
	DSN                                                              string
	Output                                                           string
	Agents, Candidates, Active, Rules, Replicas, Batch, Pool, Budget int
	Duration, Drain                                                  time.Duration
	Cleanup, Diagnostics                                             bool
}
type replica struct {
	store   *store.Store
	hub     *agenthub.Hub
	api     *api.Server
	grpc    *grpc.Server
	address string
}
type node struct {
	id, credential string
	stream         agentproto.AgentService_ConnectClient
	connection     *grpc.ClientConn
	replica        int
}
type observation struct {
	at    time.Time
	delay float64
}
type statistics struct {
	mu                sync.Mutex
	started           time.Time
	received          map[string]observation
	acked             map[string]bool
	firstActive       map[string]time.Time
	activeNodes       map[string]map[string]time.Time
	firstTarget       map[string]time.Time
	errors            []string
	batchLatency      []float64
	replay            map[string][]domain.Doc
	peakHeap, peakSys uint64
	peakGoroutines    int
	timedAcked        int64
}
type report struct {
	StartedAt, FinishedAt, Schema, Database, DatabaseVersion, GoVersion, OS, Architecture, Postgres                  string
	CPUs, Agents, Candidates, Active, Rules, Replicas, Batch, PoolPerReplica, DispatchBudget                         int
	RequestedDurationSeconds, ActualDurationSeconds, DrainSeconds, SeedSeconds                                       float64
	TasksReceived, ResultsAcknowledged, TasksPersisted, TasksDone, TasksPending, Metrics, IdempotencyIDs             int64
	DistinctActiveReceived, DistinctActiveScheduled, EvaluatedRules, HealthyRules, RulesWithAllTargetSamples         int64
	SchedulerCoverageWithin30Seconds                                                                                 int
	ResultLoss                                                                                                       int64
	TimedWindowResultsAcknowledged                                                                                   int64
	ResultThroughputPerSecond, TaskDeliveryP50MS, TaskDeliveryP95MS, TaskDeliveryP99MS, BatchAckP50MS, BatchAckP95MS float64
	PeakHeapBytes, PeakProcessSysBytes, SchemaBytes                                                                  uint64
	PeakGoroutines                                                                                                   int
	ReplayAttempts, ReplayErrors, ReplayMetricGrowth                                                                 int64
	ReplaySeconds                                                                                                    float64
	ConcurrentInsertAttempts, ConcurrentInsertExpectedMetrics, ConcurrentInsertMetricGrowth, ConcurrentInsertErrors  int64
	ConcurrentInsertSeconds                                                                                          float64
	AllResultsAcknowledged, AllActiveWithinOneCycle, AllRulesEvaluated, AllRulesHealthy, IdempotentReplay            bool
	LockDiagnosticsEnabled                                                                                           bool
	Errors, Exclusions                                                                                               []string
}

func main() {
	if err := run(); err != nil {
		slog.Error("load test failed", "error", err)
		os.Exit(1)
	}
}
func run() error {
	var c config
	defaultDSN := os.Getenv("NEWNEXUS_TEST_MYSQL")
	if defaultDSN == "" {
		defaultDSN = os.Getenv("NEWNEXUS_TEST_POSTGRES")
	}
	flag.StringVar(&c.DSN, "database-url", defaultDSN, "Dedicated MySQL 8.4 URL; always creates a new isolated database (PostgreSQL uses a schema)")
	flag.StringVar(&c.Output, "output", "artifacts/performance.json", "JSON report output")
	flag.IntVar(&c.Agents, "agents", 100, "Concurrent outbound gRPC node connections")
	flag.IntVar(&c.Candidates, "candidates", 100000, "Total inventory candidates")
	flag.IntVar(&c.Active, "active", 10000, "Candidates due for 30-second active checks")
	flag.IntVar(&c.Rules, "rules", 1000, "Rules, each with two targets and two-node quorum")
	flag.IntVar(&c.Replicas, "replicas", 2, "Control-plane/gRPC replicas sharing the database")
	flag.IntVar(&c.Batch, "batch", 64, "Maximum observations per real gRPC results batch")
	flag.IntVar(&c.Pool, "db-connections", 32, "Maximum database connections per replica")
	flag.IntVar(&c.Budget, "dispatch-budget", 32, "probe.concurrency scheduler setting (production default 32)")
	flag.DurationVar(&c.Duration, "duration", 60*time.Second, "Timed production scheduler run")
	flag.DurationVar(&c.Drain, "drain", 30*time.Second, "Additional time for queued work and ACKs")
	flag.BoolVar(&c.Cleanup, "cleanup", false, "Drop only the randomly created test database/schema after writing its report")
	flag.BoolVar(&c.Diagnostics, "diagnostics", false, "Observe MySQL metadata/row lock waits once per second (diagnostic runs only)")
	flag.Parse()
	if c.Agents < 2 || c.Agents > 1000 || c.Candidates < c.Active || c.Active < 1 || c.Candidates > 1000000 || c.Rules < 0 || c.Rules > 10000 || c.Replicas < 1 || c.Replicas > 8 || c.Batch < 1 || c.Batch > 256 || c.Pool < 1 || c.Pool > 128 || c.Budget < 1 || c.Duration < 5*time.Second || c.Drain < 0 {
		return errors.New("invalid workload limits")
	}
	u, err := url.Parse(c.DSN)
	if err != nil || (u.Scheme != "mysql" && u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return errors.New("database-url must be a dedicated MySQL or PostgreSQL URL")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	schema := "newnexus_load_" + randomID()[:16]
	admin, err := openAdministrator(c.DSN)
	if err != nil {
		return err
	}
	defer admin.Close()
	create, drop := `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`
	if u.Scheme == "mysql" {
		create = "CREATE DATABASE `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"
		drop = "DROP DATABASE `" + schema + "`"
	}
	if _, err := admin.ExecContext(ctx, create); err != nil {
		return err
	}
	if c.Cleanup {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := admin.ExecContext(cleanup, drop)
			if err != nil {
				slog.Error("test schema cleanup failed", "schema", schema, "error", err)
			}
		}()
	}
	if u.Scheme == "mysql" {
		u.Path = "/" + schema
	} else {
		query := u.Query()
		query.Set("search_path", schema)
		u.RawQuery = query.Encode()
	}
	dsn := u.String()
	var replicas []*replica
	defer func() {
		for _, r := range replicas {
			r.grpc.Stop()
			r.store.DB.Close()
		}
	}()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	vault, err := security.NewVault(key)
	if err != nil {
		return err
	}
	for i := 0; i < c.Replicas; i++ {
		db, err := store.Open(dsn)
		if err != nil {
			return err
		}
		db.DB.SetMaxOpenConns(c.Pool)
		db.DB.SetMaxIdleConns(min(c.Pool, 8))
		hub := agenthub.New(db)
		hub.Vault = vault
		server := api.New(db, vault, hub, control.New(db, vault))
		if err := server.EnsureDefaults(ctx); err != nil {
			return err
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		g := grpc.NewServer()
		agentproto.RegisterAgentServiceServer(g, hub)
		go g.Serve(listener)
		replicas = append(replicas, &replica{store: db, hub: hub, api: server, grpc: g, address: listener.Addr().String()})
	}
	db := replicas[0].store
	stats := &statistics{received: map[string]observation{}, acked: map[string]bool{}, firstActive: map[string]time.Time{}, activeNodes: map[string]map[string]time.Time{}, firstTarget: map[string]time.Time{}, replay: map[string][]domain.Doc{}}
	clientsCtx, cancelClients := context.WithCancel(ctx)
	defer cancelClients()
	var nodes []node
	for i := 0; i < c.Agents; i++ {
		n, err := connectNode(clientsCtx, replicas[i%c.Replicas], i)
		if err != nil {
			return fmt.Errorf("register node %d: %w", i, err)
		}
		n.replica = i % c.Replicas
		nodes = append(nodes, n)
	}
	defer func() {
		for _, n := range nodes {
			n.connection.Close()
		}
	}()
	seedStarted := time.Now()
	if err := seed(ctx, db, dsn, c, nodes); err != nil {
		return err
	}
	seedDuration := time.Since(seedStarted)
	if err := setSetting(ctx, db, "probe.concurrency", c.Budget); err != nil {
		return err
	}
	var clients sync.WaitGroup
	for _, n := range nodes {
		if err := send(n.stream, map[string]any{"type": "heartbeat", "node_id": n.id, "version": "loadtest", "capabilities": []any{"tcp", "http"}}); err != nil {
			return err
		}
		clients.Add(1)
		go func(n node) { defer clients.Done(); exerciseNode(clientsCtx, n, c.Batch, stats) }(n)
	}
	// StartWorkers is the same entry point used by cmd/server; all replicas
	// compete for the production scheduler/job leases.
	stats.started = time.Now().UTC()
	diagnosticsCtx, cancelDiagnostics := context.WithCancel(ctx)
	defer cancelDiagnostics()
	if c.Diagnostics && db.Driver == "mysql" {
		go observeMySQL(diagnosticsCtx, admin, schema)
	}
	workersCtx, cancelWorkers := context.WithCancel(ctx)
	for _, r := range replicas {
		r.api.StartWorkers(workersCtx)
	}
	slog.Info("timed workload started", "schema", schema, "agents", c.Agents, "candidates", c.Candidates, "active", c.Active, "rules", c.Rules, "replicas", c.Replicas, "duration", c.Duration)
	runStarted := time.Now()
	measureCtx, cancelMeasure := context.WithCancel(ctx)
	go sampleMemory(measureCtx, stats)
	timer := time.NewTimer(c.Duration)
	select {
	case <-ctx.Done():
		timer.Stop()
	case <-timer.C:
	}
	cancelWorkers()
	actualDuration := time.Since(runStarted)
	stats.mu.Lock()
	stats.timedAcked = int64(len(stats.acked))
	stats.mu.Unlock()
	deadline := time.Now().Add(c.Drain)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		var pending int64
		err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM resources WHERE kind='tasks' AND `+jsonText(db, "status")+`<>'done'`).Scan(&pending)
		if err != nil {
			stats.recordError(err)
			break
		}
		stats.mu.Lock()
		allAcked := len(stats.received) == len(stats.acked)
		stats.mu.Unlock()
		if pending == 0 && allAcked {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	cancelMeasure()
	cancelClients()
	clients.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	report := finishReport(ctx, c, db, stats, schema, seedDuration, actualDuration)
	_ = admin.QueryRowContext(ctx, `SELECT version()`).Scan(&report.DatabaseVersion)
	if db.Driver == "pgx" {
		report.Postgres = report.DatabaseVersion
	}
	replayStarted := time.Now()
	replayDuplicates(ctx, replicas, stats, &report)
	report.ReplaySeconds = time.Since(replayStarted).Seconds()
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writeReport(c.Output, report); err != nil {
		return err
	}
	slog.Info("load test report written", "path", c.Output, "received", report.TasksReceived, "acked", report.ResultsAcknowledged, "pending", report.TasksPending, "active_within_30s", report.SchedulerCoverageWithin30Seconds, "replay_growth", report.ReplayMetricGrowth)
	if !report.AllResultsAcknowledged || !report.AllActiveWithinOneCycle || !report.AllRulesEvaluated || !report.AllRulesHealthy || !report.IdempotentReplay {
		return errors.New("one or more production acceptance targets were not met; see the JSON report")
	}
	return nil
}

func connectNode(ctx context.Context, r *replica, index int) (node, error) {
	token, err := r.hub.Enrollment(ctx, time.Hour, fmt.Sprintf("load-node-%03d", index))
	if err != nil {
		return node{}, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, r.address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return node{}, err
	}
	client := agentproto.NewAgentServiceClient(conn)
	in, _ := structpb.NewStruct(map[string]any{"enrollment_token": token["token"], "name": fmt.Sprintf("load-node-%03d", index), "region": "loadtest", "version": "loadtest", "protocol_version": 1})
	enrolled, err := client.Enroll(ctx, in)
	if err != nil {
		conn.Close()
		return node{}, err
	}
	m := enrolled.AsMap()
	id, _ := m["node_id"].(string)
	secret, _ := m["credential"].(string)
	stream, err := client.Connect(ctx)
	if err != nil {
		conn.Close()
		return node{}, err
	}
	if err := send(stream, map[string]any{"type": "hello", "node_id": id, "credential": secret, "protocol_version": 1, "version": "loadtest"}); err != nil {
		conn.Close()
		return node{}, err
	}
	first, err := stream.Recv()
	if err != nil {
		conn.Close()
		return node{}, err
	}
	if first.AsMap()["type"] != "config" {
		conn.Close()
		return node{}, errors.New("server omitted config handshake")
	}
	if err := send(stream, map[string]any{"type": "heartbeat", "node_id": id, "version": "loadtest", "capabilities": []any{"tcp", "http"}}); err != nil {
		conn.Close()
		return node{}, err
	}
	return node{id: id, credential: secret, stream: stream, connection: conn}, nil
}

func seed(ctx context.Context, db *store.Store, dsn string, c config, nodes []node) error {
	var conn *pgx.Conn
	if db.Driver == "pgx" {
		var err error
		conn, err = pgx.Connect(ctx, dsn)
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
	}
	now := db.Time(time.Now())
	old := db.Time(time.Now().Add(-31 * time.Second))
	rows := make([][]any, 0, 500)
	flush := func() error {
		if len(rows) == 0 {
			return nil
		}
		var err error
		if conn != nil {
			_, err = conn.CopyFrom(ctx, pgx.Identifier{"resources"}, []string{"kind", "id", "doc", "created_at", "updated_at"}, pgx.CopyFromRows(rows))
		} else {
			marks := make([]string, 0, len(rows))
			args := make([]any, 0, len(rows)*5)
			for _, row := range rows {
				marks = append(marks, "(?,?,?,?,?)")
				args = append(args, row...)
			}
			_, err = db.DB.ExecContext(ctx, db.Q(`INSERT INTO resources(kind,id,doc,created_at,updated_at) VALUES `+strings.Join(marks, ",")), args...)
		}
		rows = rows[:0]
		return err
	}
	add := func(kind string, d domain.Doc) error {
		d["created_at"] = now
		d["updated_at"] = now
		data, err := json.Marshal(d)
		if err != nil {
			return err
		}
		rows = append(rows, []any{kind, d["id"], string(data), now, now})
		if len(rows) == cap(rows) {
			return flush()
		}
		return nil
	}
	if err := add("projects", domain.Doc{"id": "load-project", "name": "Isolated performance fixture", "status": "active"}); err != nil {
		return err
	}
	if err := add("probe-profiles", domain.Doc{"id": "load-profile", "project_id": "load-project", "name": "Synthetic observations only", "mode": "http", "url": "https://load.invalid/health", "samples": 3, "timeout_ms": 5000, "max_bytes": 1024}); err != nil {
		return err
	}
	for i := 0; i < c.Candidates; i++ {
		tier := "inventory"
		last := now
		if i < c.Active {
			tier = "active"
			last = old
		}
		d := domain.Doc{"id": fmt.Sprintf("load-candidate-%06d", i), "project_id": "load-project", "address": fmt.Sprintf("104.%d.%d.%d", 16+(i/65536)%16, (i/256)%256, i%256), "tier": tier, "family": 4, "last_scheduled_at": last, "healthy": true}
		if err := add("candidates", d); err != nil {
			return err
		}
	}
	groups := max(1, len(nodes)/2)
	for i := 0; i < groups; i++ {
		if err := add("node-groups", domain.Doc{"id": fmt.Sprintf("load-group-%03d", i), "name": fmt.Sprintf("load-group-%03d", i), "node_ids": []any{nodes[(2*i)%len(nodes)].id, nodes[(2*i+1)%len(nodes)].id}}); err != nil {
			return err
		}
	}
	for i := 0; i < c.Rules; i++ {
		primary := fmt.Sprintf("load-target-%04d-a", i)
		backup := fmt.Sprintf("load-target-%04d-b", i)
		line := domain.Doc{"id": fmt.Sprintf("load-line-%04d", i), "provider_line_code": "default", "node_group": fmt.Sprintf("load-group-%03d", i%groups), "min_nodes": 2, "quorum_percent": 67, "active_priority": 1, "selected_target_ids": []any{primary}, "targets": []any{domain.Doc{"id": primary, "name": "primary", "cname": "primary.load.invalid", "enabled": true, "priority": 1, "weight": 100}, domain.Doc{"id": backup, "name": "backup", "cname": "backup.load.invalid", "enabled": true, "priority": 2, "weight": 100}}}
		if err := add("rules", domain.Doc{"id": fmt.Sprintf("load-rule-%04d", i), "project_id": "load-project", "name": fmt.Sprintf("Load rule %d", i), "hostname": fmt.Sprintf("load-%04d.invalid", i), "probe_profile_id": "load-profile", "publish_mode": "manual", "switch_mode": "availability", "status": "healthy", "last_probe_scheduled_at": old, "lines": []any{line}}); err != nil {
			return err
		}
	}
	return flush()
}

func openAdministrator(dsn string) (*sql.DB, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "mysql" {
		return sql.Open("pgx", dsn)
	}
	cfg, err := store.MySQLConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.DBName = ""
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(connector), nil
}

func jsonText(db *store.Store, key string) string {
	// Only internal field names are passed; no user-supplied SQL identifiers.
	if db.Driver == "mysql" {
		return `NULLIF(JSON_UNQUOTE(JSON_EXTRACT(doc,'$.` + key + `')),'null')`
	}
	if db.Driver == "pgx" {
		return `doc::jsonb->>'` + key + `'`
	}
	return `json_extract(doc,'$.` + key + `')`
}

func setSetting(ctx context.Context, db *store.Store, key string, value any) error {
	d, err := db.Get(ctx, "settings", key)
	if err != nil {
		return err
	}
	d["value"] = value
	return db.Put(ctx, "settings", d)
}
func send(stream agentproto.AgentService_ConnectClient, value map[string]any) error {
	m, err := structpb.NewStruct(value)
	if err != nil {
		return err
	}
	return stream.Send(m)
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type frame struct {
	m   *structpb.Struct
	err error
}

func exerciseNode(ctx context.Context, n node, batchSize int, stats *statistics) {
	incoming := make(chan frame, 64)
	go func() {
		for {
			m, err := n.stream.Recv()
			select {
			case incoming <- frame{m, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer n.stream.CloseSend()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	flushTimer := time.NewTicker(100 * time.Millisecond)
	defer flushTimer.Stop()
	var queue []domain.Doc
	var inflight []domain.Doc
	var inflightID string
	var batchSent time.Time
	seen := map[string]bool{}
	flush := func() error {
		if len(inflight) > 0 || len(queue) == 0 {
			return nil
		}
		size := min(batchSize, len(queue))
		inflight = append([]domain.Doc(nil), queue[:size]...)
		queue = queue[size:]
		inflightID = randomID()
		wire := make([]any, len(inflight))
		for i, d := range inflight {
			wire[i] = map[string]any(d)
		}
		batchSent = time.Now()
		return send(n.stream, map[string]any{"type": "results", "batch_id": inflightID, "results": wire})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := send(n.stream, map[string]any{"type": "heartbeat", "node_id": n.id, "version": "loadtest", "capabilities": []any{"tcp", "http"}}); err != nil {
				if ctx.Err() == nil {
					stats.recordError(err)
				}
				return
			}
		case <-flushTimer.C:
			if err := flush(); err != nil {
				if ctx.Err() == nil {
					stats.recordError(err)
				}
				return
			}
		case received := <-incoming:
			if received.err != nil {
				if ctx.Err() == nil {
					stats.recordError(received.err)
				}
				return
			}
			m := received.m.AsMap()
			switch m["type"] {
			case "task":
				task, ok := m["task"].(map[string]any)
				if !ok {
					stats.recordError(errors.New("invalid task frame"))
					return
				}
				id, _ := task["id"].(string)
				if err := send(n.stream, map[string]any{"type": "ack", "task_id": id}); err != nil {
					stats.recordError(err)
					return
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				now := time.Now().UTC()
				candidate, _ := task["candidate_id"].(string)
				created, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(task["created_at"]))
				delay := float64(now.Sub(created)) / float64(time.Millisecond)
				if created.IsZero() {
					delay = 0
				}
				stats.mu.Lock()
				stats.received[id] = observation{now, delay}
				if strings.HasPrefix(candidate, "load-candidate-") {
					if _, ok := stats.firstActive[candidate]; !ok {
						stats.firstActive[candidate] = now
					}
					if stats.activeNodes[candidate] == nil {
						stats.activeNodes[candidate] = map[string]time.Time{}
					}
					if _, ok := stats.activeNodes[candidate][n.id]; !ok {
						stats.activeNodes[candidate][n.id] = now
					}
				}
				if strings.HasPrefix(candidate, "target:") {
					stats.firstTarget[candidate+":"+n.id] = now
				}
				stats.mu.Unlock()
				queue = append(queue, domain.Doc{"id": agentclient.ResultID(n.id, id), "task_id": id, "candidate_id": candidate, "address": task["address"], "observed_at": now.Format(time.RFC3339Nano), "success": true, "tcp_ms": 20, "tls_ms": 15, "ttfb_ms": 50, "p50_ms": 50, "p95_ms": 60, "download_bps": 1048576, "sample_count": 3, "success_rate": 1, "requested_samples": 3, "attempted_samples": 3})
				if len(queue) >= batchSize {
					if err := flush(); err != nil {
						stats.recordError(err)
						return
					}
				}
			case "ack":
				if len(inflight) > 0 && m["batch_id"] == inflightID {
					stats.mu.Lock()
					for _, d := range inflight {
						stats.acked[domain.S(d, "task_id")] = true
					}
					stats.batchLatency = append(stats.batchLatency, float64(time.Since(batchSent))/float64(time.Millisecond))
					if len(stats.replay[n.id]) == 0 {
						stats.replay[n.id] = append([]domain.Doc(nil), inflight...)
					}
					stats.mu.Unlock()
					inflight = nil
					if err := flush(); err != nil {
						stats.recordError(err)
						return
					}
				}
			}
		}
	}
}

func (s *statistics) recordError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errors) < 100 {
		s.errors = append(s.errors, security.Redact(err.Error()))
	}
}
func sampleMemory(ctx context.Context, s *statistics) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			s.mu.Lock()
			s.peakHeap = max(s.peakHeap, m.HeapAlloc)
			s.peakSys = max(s.peakSys, m.Sys)
			s.peakGoroutines = max(s.peakGoroutines, runtime.NumGoroutine())
			s.mu.Unlock()
		}
	}
}

func finishReport(ctx context.Context, c config, db *store.Store, s *statistics, schema string, seedDuration, actualDuration time.Duration) report {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := report{StartedAt: s.started.Format(time.RFC3339), Schema: schema, GoVersion: runtime.Version(), OS: runtime.GOOS, Architecture: runtime.GOARCH, CPUs: runtime.NumCPU(), Agents: c.Agents, Candidates: c.Candidates, Active: c.Active, Rules: c.Rules, Replicas: c.Replicas, Batch: c.Batch, PoolPerReplica: c.Pool, DispatchBudget: c.Budget, RequestedDurationSeconds: c.Duration.Seconds(), ActualDurationSeconds: actualDuration.Seconds(), DrainSeconds: c.Drain.Seconds(), SeedSeconds: seedDuration.Seconds(), TasksReceived: int64(len(s.received)), ResultsAcknowledged: int64(len(s.acked)), DistinctActiveReceived: int64(len(s.firstActive)), PeakHeapBytes: s.peakHeap, PeakProcessSysBytes: s.peakSys, PeakGoroutines: s.peakGoroutines, Errors: append([]string(nil), s.errors...), Exclusions: []string{"Synthetic probe observations: no Internet/CDN bandwidth or 100 separate node hosts measured", "gRPC loopback plaintext: TLS correctness is tested separately, not TLS performance", "Manual DNS publication: no DNS-provider quota, external writes or automatic failover publication load", "No disk-outbox latency on simulated clients; production Agent outbox correctness is tested separately", "Replicas are goroutines in one process, using distinct stores/hubs and shared database leases; not separate machines"}}
	delays := make([]float64, 0, len(s.received))
	for _, d := range s.received {
		delays = append(delays, d.delay)
	}
	r.TaskDeliveryP50MS = quantile(delays, .5)
	r.TaskDeliveryP95MS = quantile(delays, .95)
	r.TaskDeliveryP99MS = quantile(delays, .99)
	r.BatchAckP50MS = quantile(s.batchLatency, .5)
	r.BatchAckP95MS = quantile(s.batchLatency, .95)
	for _, byNode := range s.activeNodes {
		onTime := 0
		for _, at := range byNode {
			if at.Sub(s.started) <= 30*time.Second {
				onTime++
			}
		}
		if onTime >= min(c.Agents, 3) {
			r.SchedulerCoverageWithin30Seconds++
		}
	}
	queries := []struct {
		q    string
		dest *int64
	}{{`SELECT COUNT(*) FROM resources WHERE kind='tasks'`, &r.TasksPersisted}, {`SELECT COUNT(*) FROM resources WHERE kind='tasks' AND ` + jsonText(db, "status") + `='done'`, &r.TasksDone}, {`SELECT COUNT(*) FROM resources WHERE kind='tasks' AND ` + jsonText(db, "status") + `<>'done'`, &r.TasksPending}, {`SELECT COUNT(*) FROM metrics`, &r.Metrics}, {`SELECT COUNT(*) FROM result_ids`, &r.IdempotencyIDs}, {`SELECT COUNT(*) FROM resources WHERE kind='rules' AND ` + jsonText(db, "status") + `='healthy' AND ` + jsonText(db, "updated_at") + `>=?`, &r.HealthyRules}, {`SELECT COUNT(*) FROM resources WHERE kind='candidates' AND ` + jsonText(db, "tier") + `='active' AND ` + jsonText(db, "last_scheduled_at") + `>=?`, &r.DistinctActiveScheduled}}
	for _, q := range queries {
		var err error
		if strings.Contains(q.q, "?") {
			err = db.DB.QueryRowContext(ctx, db.Q(q.q), db.Time(s.started)).Scan(q.dest)
		} else {
			err = db.DB.QueryRowContext(ctx, q.q).Scan(q.dest)
		}
		if err != nil {
			r.Errors = append(r.Errors, security.Redact(err.Error()))
		}
	}
	rules, err := db.List(ctx, "rules")
	if err != nil {
		r.Errors = append(r.Errors, security.Redact(err.Error()))
	}
	for _, rule := range rules {
		lines := domain.A(rule, "lines")
		evaluated := len(lines) > 0
		for _, line := range lines {
			// Fresh fixtures omit this field; routing.Evaluate sets it on both
			// insufficient-quorum and healthy decisions. Count actual evaluation
			// separately from whether enough measurements arrived to be healthy.
			if _, exists := line["auto_switch_paused"]; !exists {
				evaluated = false
			}
		}
		if evaluated {
			r.EvaluatedRules++
		}
	}
	targetCounts := map[string]int{}
	for key := range s.firstTarget {
		if separator := strings.LastIndex(key, ":"); separator >= 0 {
			targetCounts[key[:separator]]++
		}
	}
	for i := 0; i < c.Rules; i++ {
		covered := true
		for _, suffix := range []string{"a", "b"} {
			if targetCounts[fmt.Sprintf("target:load-target-%04d-%s", i, suffix)] < 2 {
				covered = false
			}
		}
		if covered {
			r.RulesWithAllTargetSamples++
		}
	}
	r.Database = db.Driver
	r.LockDiagnosticsEnabled = c.Diagnostics
	if c.Diagnostics {
		r.Exclusions = append(r.Exclusions, "Opt-in MySQL lock metadata sampling was enabled; diagnostic run, requires a separate run without this observer for final performance acceptance")
	}
	if db.Driver == "mysql" {
		_ = db.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(DATA_LENGTH+INDEX_LENGTH),0) FROM information_schema.TABLES WHERE TABLE_SCHEMA=?`, schema).Scan(&r.SchemaBytes)
	} else {
		_ = db.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(pg_total_relation_size(c.oid)),0) FROM pg_class c JOIN pg_namespace n ON c.relnamespace=n.oid WHERE n.nspname=$1 AND c.relkind IN ('r','m')`, schema).Scan(&r.SchemaBytes)
	}
	r.TimedWindowResultsAcknowledged = s.timedAcked
	r.ResultLoss = r.TasksReceived - r.ResultsAcknowledged
	r.ResultThroughputPerSecond = float64(s.timedAcked) / actualDuration.Seconds()
	r.AllResultsAcknowledged = r.ResultLoss == 0 && r.TasksPending == 0 && r.Metrics == r.ResultsAcknowledged && len(r.Errors) == 0
	r.AllActiveWithinOneCycle = r.SchedulerCoverageWithin30Seconds == c.Active
	r.AllRulesEvaluated = r.EvaluatedRules == int64(c.Rules)
	r.AllRulesHealthy = r.HealthyRules == int64(c.Rules)
	return r
}

func replayDuplicates(ctx context.Context, replicas []*replica, s *statistics, r *report) {
	var before, after int64
	_ = replicas[0].store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM metrics`).Scan(&before)
	s.mu.Lock()
	batches := map[string][]domain.Doc{}
	for nodeID, batch := range s.replay {
		batches[nodeID] = append([]domain.Doc(nil), batch...)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for nodeID, batch := range batches {
		for _, replica := range replicas {
			wg.Add(1)
			go func(h *agenthub.Hub, nodeID string, batch []domain.Doc) {
				defer wg.Done()
				err := h.Ingest(ctx, nodeID, batch)
				mu.Lock()
				r.ReplayAttempts += int64(len(batch))
				if err != nil {
					r.ReplayErrors++
					if len(r.Errors) < 100 {
						r.Errors = append(r.Errors, security.Redact(err.Error()))
					}
				}
				mu.Unlock()
			}(replica.hub, nodeID, batch)
		}
	}
	wg.Wait()
	if err := replicas[0].store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM metrics`).Scan(&after); err != nil {
		r.Errors = append(r.Errors, security.Redact(err.Error()))
	}
	r.ReplayMetricGrowth = after - before
	r.IdempotentReplay = r.ReplayAttempts > 0 && r.ReplayErrors == 0 && r.ReplayMetricGrowth == 0
	// Race genuinely new result IDs through independent Hub instances. Merely
	// replaying already-committed IDs would miss INSERT ON CONFLICT races.
	fresh := map[string][]domain.Doc{}
	for nodeID, batch := range batches {
		var tasks []domain.Doc
		for i, d := range batch {
			if i >= 8 {
				break
			}
			copy := domain.Clone(d)
			taskID := "load-race-" + domain.ID()
			copy["task_id"] = taskID
			copy["id"] = agentclient.ResultID(nodeID, taskID)
			copy["observed_at"] = domain.Now()
			fresh[nodeID] = append(fresh[nodeID], copy)
			tasks = append(tasks, domain.Doc{"id": taskID, "node_id": nodeID, "candidate_id": copy["candidate_id"], "address": copy["address"], "status": "queued"})
			r.ConcurrentInsertExpectedMetrics++
		}
		if err := replicas[0].store.PutMany(ctx, "tasks", tasks); err != nil {
			r.Errors = append(r.Errors, security.Redact(err.Error()))
			r.ConcurrentInsertErrors++
		}
	}
	raceStarted := time.Now()
	before = after
	for nodeID, batch := range fresh {
		for _, replica := range replicas {
			wg.Add(1)
			go func(h *agenthub.Hub, nodeID string, batch []domain.Doc) {
				defer wg.Done()
				err := h.Ingest(ctx, nodeID, batch)
				mu.Lock()
				r.ConcurrentInsertAttempts += int64(len(batch))
				if err != nil {
					r.ConcurrentInsertErrors++
					if len(r.Errors) < 100 {
						r.Errors = append(r.Errors, security.Redact(err.Error()))
					}
				}
				mu.Unlock()
			}(replica.hub, nodeID, batch)
		}
	}
	wg.Wait()
	r.ConcurrentInsertSeconds = time.Since(raceStarted).Seconds()
	if err := replicas[0].store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM metrics`).Scan(&after); err != nil {
		r.Errors = append(r.Errors, security.Redact(err.Error()))
	}
	r.ConcurrentInsertMetricGrowth = after - before
	r.IdempotentReplay = r.IdempotentReplay && r.ConcurrentInsertErrors == 0 && r.ConcurrentInsertMetricGrowth == r.ConcurrentInsertExpectedMetrics
}
func quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	rows := append([]float64(nil), values...)
	sort.Float64s(rows)
	index := int(math.Ceil(float64(len(rows))*q)) - 1
	index = max(0, min(index, len(rows)-1))
	return rows[index]
}
func writeReport(path string, value report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}
