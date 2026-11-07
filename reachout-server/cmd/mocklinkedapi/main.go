// Command mocklinkedapi is a local stand-in for Linked API, for running Reachout without a
// Linked API account: development, demos and the live check. It answers just enough of the
// admin and workflow endpoints for the connect flow and lead search. It never contacts LinkedIn.
//
//	go run ./cmd/mocklinkedapi -addr :18090
//	LINKED_API_BASE_URL=http://localhost:18090 LINKED_API_TOKEN=anything go run ./cmd/server
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type account struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	URL                 string  `json:"url"`
	AvatarURL           *string `json:"avatarUrl"`
	Headline            string  `json:"headline"`
	CountryCode         string  `json:"countryCode"`
	IdentificationToken string  `json:"identificationToken"`
	Status              string  `json:"status"`
	ConnectedAt         string  `json:"connectedAt"`
}

type session struct {
	polls int
	done  bool
}

type mock struct {
	mu        sync.Mutex
	n         int
	sessions  map[string]*session
	accounts  []account
	workflows map[string]map[string]any
}

func newMock() *mock {
	return &mock{sessions: map[string]*session{}, workflows: map[string]map[string]any{}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, result any) {
	writeJSON(w, 200, map[string]any{"success": true, "result": result})
}

func fail(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "error": map[string]string{"type": typ, "message": msg}})
}

func (m *mock) handler(base string) http.Handler {
	mux := http.NewServeMux()
	// Every request must carry the admin token, as the real API requires.
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("linked-api-token") == "" {
				fail(w, 401, "linkedApiTokenRequired", "linked-api-token header is required")
				return
			}
			next(w, r)
		}
	}
	var body = func(r *http.Request) map[string]any {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		return b
	}

	mux.HandleFunc("POST /admin/accounts.createConnectionSession", guard(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.n++
		id := fmt.Sprintf("mock-sess-%d", m.n)
		m.sessions[id] = &session{}
		ok(w, map[string]string{"sessionId": id, "connectionLink": base + "/connection/" + id})
	}))

	// The "user" finishes logging in on the third check, and the account then exists.
	mux.HandleFunc("POST /admin/accounts.getConnectionSession", guard(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id, _ := body(r)["sessionId"].(string)
		s, found := m.sessions[id]
		if !found {
			fail(w, 404, "sessionNotFound", "session not found")
			return
		}
		s.polls++
		status := "pending"
		switch {
		case s.polls == 2:
			status = "streaming"
		case s.polls >= 3:
			status = "success"
			if !s.done {
				s.done = true
				n := len(m.accounts) + 1
				m.accounts = append(m.accounts, account{
					ID: fmt.Sprintf("mock-acct-%d", n), Name: fmt.Sprintf("Mock User %d", n),
					URL: fmt.Sprintf("https://www.linkedin.com/in/mock-user-%d", n), Headline: "Founder at Example Co",
					CountryCode: "IN", IdentificationToken: fmt.Sprintf("mock-token-%d", n), Status: "active",
					ConnectedAt: time.Now().UTC().Format(time.RFC3339),
				})
			}
		}
		ok(w, map[string]any{"session": map[string]string{"sessionId": id, "status": status, "type": "initial"}})
	}))

	mux.HandleFunc("POST /admin/accounts.getAll", guard(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		ok(w, map[string]any{"accounts": append([]account{}, m.accounts...), "pendingConnectionSessions": []any{}})
	}))
	mux.HandleFunc("POST /admin/accounts.cancelConnectionSession", guard(func(w http.ResponseWriter, r *http.Request) { ok(w, map[string]any{}) }))
	mux.HandleFunc("POST /admin/accounts.createReconnectionSession", guard(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.n++
		id := fmt.Sprintf("mock-resess-%d", m.n)
		ok(w, map[string]string{"reconnectionSessionId": id, "reconnectionLink": base + "/connection/" + id})
	}))
	mux.HandleFunc("POST /admin/webhook.deliveries", guard(func(w http.ResponseWriter, r *http.Request) { ok(w, map[string]any{"deliveries": []any{}}) }))
	mux.HandleFunc("POST /admin/webhook.replayDelivery", guard(func(w http.ResponseWriter, r *http.Request) { ok(w, map[string]any{}) }))

	mux.HandleFunc("POST /workflows", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("identification-token") == "" {
			fail(w, 401, "accountIdRequired", "identification-token header is required")
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.n++
		id := fmt.Sprintf("mock-wf-%d", m.n)
		m.workflows[id] = body(r)
		ok(w, map[string]any{"workflowId": id, "workflowStatus": "pending", "pendingReason": "queued", "message": "scheduled"})
	}))

	// Workflows complete on the first check. Searches return `limit` made-up people.
	mux.HandleFunc("GET /workflows/{id}", guard(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		action, found := m.workflows[r.PathValue("id")]
		if !found {
			fail(w, 404, "workflowNotFound", "workflow not found")
			return
		}
		kind, _ := action["actionType"].(string)
		completion := map[string]any{"actionType": kind, "success": true}
		if kind == "st.searchPeople" {
			limit := 10
			if l, isNum := action["limit"].(float64); isNum {
				limit = int(l)
			}
			people := make([]map[string]any, 0, limit)
			for i := 1; i <= limit; i++ {
				people = append(people, map[string]any{
					"name": fmt.Sprintf("Searched Person %d", i), "urn": fmt.Sprintf("urn:li:member:%d", 1000+i),
					"publicUrl": fmt.Sprintf("https://www.linkedin.com/in/searched-person-%d", i),
					"headline":  "CTO at Example Co", "location": "London",
				})
			}
			completion["data"] = people
		}
		ok(w, map[string]any{"workflowStatus": "completed", "pendingReason": nil, "completion": completion})
	}))
	return mux
}

func main() {
	addr := flag.String("addr", ":18090", "listen address")
	flag.Parse()
	base := "http://localhost" + *addr
	if !strings.HasPrefix(*addr, ":") {
		base = "http://" + *addr
	}
	log.Printf("mock Linked API on %s (accounts connect after three status checks; searches return made-up people)", *addr)
	log.Fatal(http.ListenAndServe(*addr, newMock().handler(base)))
}
