package api

import (
	"container/list"
	"sync"
	"time"
)

const (
	loginAttemptLimit  = 10
	loginAttemptWindow = 5 * time.Minute
	maxLoginClients    = 10000
	loginCleanupPeriod = time.Minute
)

type loginClient struct {
	key      string
	attempts []time.Time
}

type loginLimiter struct {
	mu          sync.Mutex
	clients     map[string]*list.Element
	recent      list.List
	nextCleanup time.Time
}

func (l *loginLimiter) remove(element *list.Element) {
	delete(l.clients, element.Value.(*loginClient).key)
	l.recent.Remove(element)
}

// allowLogin records only admitted attempts. Rejected requests cannot extend
// the retry window or keep a record recent. At capacity, a new client evicts
// the least recently admitted client instead of blocking all new logins.
func (s *Server) allowLogin(client string, now time.Time) (bool, time.Duration) {
	limiter := &s.loginLimiter
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.clients == nil {
		limiter.clients = make(map[string]*list.Element)
	}
	if !now.Before(limiter.nextCleanup) {
		// The map contains at most maxLoginClients entries; cleanup runs at
		// most once per minute, while normal admission and eviction are O(1).
		for _, element := range limiter.clients {
			history := element.Value.(*loginClient).attempts
			if len(history) == 0 || !history[len(history)-1].Add(loginAttemptWindow).After(now) {
				limiter.remove(element)
			}
		}
		limiter.nextCleanup = now.Add(loginCleanupPeriod)
	}
	element := limiter.clients[client]
	if element == nil {
		if len(limiter.clients) >= maxLoginClients {
			limiter.remove(limiter.recent.Back())
		}
		element = limiter.recent.PushFront(&loginClient{key: client, attempts: make([]time.Time, 0, loginAttemptLimit)})
		limiter.clients[client] = element
	}
	record := element.Value.(*loginClient)
	history := record.attempts
	fresh := history[:0]
	for _, at := range history {
		if at.Add(loginAttemptWindow).After(now) {
			fresh = append(fresh, at)
		}
	}
	if len(fresh) >= loginAttemptLimit {
		record.attempts = fresh
		return false, fresh[0].Add(loginAttemptWindow).Sub(now)
	}
	record.attempts = append(fresh, now)
	limiter.recent.MoveToFront(element)
	return true, 0
}
