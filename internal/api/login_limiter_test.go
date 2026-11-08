package api

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoginLimiterRejectedRequestsDoNotExtendLockout(t *testing.T) {
	s := &Server{}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < loginAttemptLimit; i++ {
		if allowed, _ := s.allowLogin("client", start); !allowed {
			t.Fatal("initial attempt was rejected")
		}
	}
	for _, elapsed := range []time.Duration{time.Minute, 4 * time.Minute, loginAttemptWindow - time.Second} {
		allowed, retry := s.allowLogin("client", start.Add(elapsed))
		history := s.loginLimiter.clients["client"].Value.(*loginClient).attempts
		if allowed || retry != loginAttemptWindow-elapsed || len(history) != loginAttemptLimit {
			t.Fatalf("rejection extended history or retry: allowed=%v, retry=%v, history=%d", allowed, retry, len(history))
		}
	}
	if allowed, _ := s.allowLogin("client", start.Add(loginAttemptWindow)); !allowed {
		t.Fatal("rejected requests kept the client locked after the admitted attempts expired")
	}
}

func TestLoginLimiterEvictsLeastRecentlyAdmittedClientAtCapacity(t *testing.T) {
	s := &Server{}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < loginAttemptLimit; i++ {
		if allowed, _ := s.allowLogin("oldest", start); !allowed {
			t.Fatal("initial oldest-client attempt was rejected")
		}
	}
	for i := 1; i < maxLoginClients; i++ {
		if allowed, _ := s.allowLogin(fmt.Sprint(i), start.Add(time.Second)); !allowed {
			t.Fatal("limiter reached capacity prematurely")
		}
	}
	if allowed, _ := s.allowLogin("oldest", start.Add(2*time.Second)); allowed {
		t.Fatal("an existing client's full window was reset at capacity")
	}
	if allowed, _ := s.allowLogin("1", start.Add(3*time.Second)); !allowed {
		t.Fatal("an admitted existing client could not update its recency")
	}
	for _, key := range []string{"new-client", "another-new-client"} {
		if allowed, _ := s.allowLogin(key, start.Add(4*time.Second)); !allowed {
			t.Fatal("a full client table blocked a new client")
		}
		if len(s.loginLimiter.clients) != maxLoginClients || s.loginLimiter.recent.Len() != maxLoginClients {
			t.Fatal("client records grew beyond the bounded table")
		}
	}
	if s.loginLimiter.clients["oldest"] != nil || s.loginLimiter.clients["2"] != nil || s.loginLimiter.clients["1"] == nil {
		t.Fatal("eviction retained a rejected client or ignored an admitted client's recency")
	}
}

func TestLoginLimiterExpiresClientRecords(t *testing.T) {
	s := &Server{}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxLoginClients; i++ {
		if allowed, _ := s.allowLogin(fmt.Sprint(i), start); !allowed {
			t.Fatal("limiter reached capacity prematurely")
		}
	}
	if allowed, _ := s.allowLogin("new-client", start.Add(loginAttemptWindow)); !allowed || len(s.loginLimiter.clients) != 1 || s.loginLimiter.recent.Len() != 1 {
		t.Fatalf("expired records were retained: allowed=%v, records=%d, recent=%d", allowed, len(s.loginLimiter.clients), s.loginLimiter.recent.Len())
	}
}

func TestProxyLoginFloodDoesNotBlockAnotherClient(t *testing.T) {
	for _, peer := range []string{"127.0.0.1:43210", "[::1]:43210", "172.17.0.1:43210", "172.18.0.2:43210"} {
		t.Run(peer, func(t *testing.T) {
			s := testServer(t)
			setup(t, s)
			post := func(client, payload string) *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(payload))
				r.RemoteAddr = peer
				r.Header.Set("X-Forwarded-For", client)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				return w
			}
			for i := 0; i < loginAttemptLimit; i++ {
				if w := post("203.0.113.1", "{"); w.Code != 400 {
					t.Fatalf("attempt %d: %d %s", i, w.Code, w.Body.String())
				}
			}
			if w := post("203.0.113.1", "{"); w.Code != 429 || w.Header().Get("Retry-After") == "" {
				t.Fatal("flooding client was not limited", w.Code, w.Body.String())
			}
			if w := post("203.0.113.2", `{"username":"admin","password":"long-password-123"}`); w.Code != 200 || len(w.Result().Cookies()) == 0 {
				t.Fatal("another client behind the same proxy could not sign in", w.Code, w.Body.String())
			}
		})
	}
}
