package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
)

func TestNotificationRecoveryKeepsOneJobAndSnapshotPerIncident(t *testing.T) {
	s := testServer(t)
	ctx := t.Context()
	for key, value := range map[string]string{"alerts.webhook_url": "https://hooks.example.com/incidents", "alerts.webhook_secret": "notification-recovery-signing-secret"} {
		setting, _ := s.Store.Get(ctx, "settings", key)
		if setting == nil {
			setting = domain.Doc{"id": key}
		}
		setting["value"] = value
		if err := s.Store.Put(ctx, "settings", setting); err != nil {
			t.Fatal(err)
		}
	}
	deliveryEvents := make(chan domain.Doc, 4)
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event domain.Doc
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		id := r.Header.Get("X-NewNexus-Event-ID")
		data := domain.M(event, "data")
		if id == "" || domain.S(event, "id") != id || domain.S(data, "incident_id") != id {
			t.Error("notification identity missing, changed or duplicated", id, event)
		}
		deliveryEvents <- domain.Doc{"id": id, "message": data["message"]}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	transport := endpoint.Client().Transport
	s.webhookClient = &http.Client{Transport: acceptanceTransport(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		fixture, _ := http.NewRequest("POST", endpoint.URL, nil)
		copy.URL = fixture.URL
		copy.Host = fixture.URL.Host
		return transport.RoundTrip(copy)
	})}
	s.Alert(ctx, "critical", "recovery-test", "Incident A", "original A", "rules", "rule")
	alerts, err := s.Store.List(ctx, "alerts")
	if err != nil || len(alerts) != 1 {
		t.Fatal(alerts, err)
	}
	alert := alerts[0]
	firstID, firstJobID := domain.S(alert, "incident_id"), domain.S(alert, "notification_job_id")
	firstJob, err := s.Store.Get(ctx, "jobs", firstJobID)
	if err != nil {
		t.Fatal(err)
	}
	// Model a crash/CAS conflict between durable job creation and stamping the
	// alert. Recovery must find that same job, using exactly the initial key.
	delete(alert, "notification_job_id")
	if err = s.Store.Put(ctx, "alerts", alert); err != nil {
		t.Fatal(err)
	}
	s.monitorExternal(ctx, time.Now())
	alert, err = s.Store.Get(ctx, "alerts", domain.S(alert, "id"))
	if err != nil || domain.S(alert, "notification_job_id") != firstJobID || s.Store.Count(ctx, "jobs") != 1 {
		t.Fatal("recovery created a second job for one incident", alert, err)
	}
	alert["status"] = "resolved"
	if err = s.Store.Put(ctx, "alerts", alert); err != nil {
		t.Fatal(err)
	}
	s.Alert(ctx, "critical", "recovery-test", "Incident B", "reopened B", "rules", "rule")
	alert, err = s.Store.Get(ctx, "alerts", domain.S(alert, "id"))
	secondID, secondJobID := domain.S(alert, "incident_id"), domain.S(alert, "notification_job_id")
	if err != nil || secondID == firstID || secondJobID == firstJobID || s.Store.Count(ctx, "jobs") != 2 {
		t.Fatal("reopening did not get an independent notification", alert, err)
	}
	secondJob, err := s.Store.Get(ctx, "jobs", secondJobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []domain.Doc{firstJob, secondJob} {
		if err = s.runJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	delivered := map[string]string{}
	for range 2 {
		select {
		case event := <-deliveryEvents:
			delivered[domain.S(event, "id")] = domain.S(event, "message")
		default:
			t.Fatal("an incident notification was not delivered")
		}
	}
	if delivered[firstID] != "original A" || delivered[secondID] != "reopened B" || len(delivered) != 2 {
		t.Fatal("queued old incident was replaced by the reopened incident", delivered)
	}
	// An old-version job without an immutable snapshot must never substitute
	// the current incident when its original incident has been superseded.
	delete(firstJob, "alert_snapshot")
	delete(firstJob, "incident_id")
	if err = s.Store.Put(ctx, "jobs", firstJob); err != nil {
		t.Fatal(err)
	}
	if err = s.runJob(ctx, firstJob); err != nil {
		t.Fatal(err)
	}
	if len(deliveryEvents) != 0 || !domain.B(domain.M(firstJob, "result"), "skipped") {
		t.Fatal("legacy job substituted a new incident", firstJob, delivered)
	}
}
