// Package panelrpc is a bounded, invocation-independent request broker.
package panelrpc

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"time"
)

const MaxBody = 16384
const MaxWire = 24576
const Retention = 60 * time.Second
const Capacity = 128

type Request struct {
	ID        string          `json:"id"`
	Epoch     string          `json:"epoch"`
	Operation string          `json:"operation"`
	Method    string          `json:"method"`
	Body      json.RawMessage `json:"body,omitempty"`
}
type Reply struct {
	ID     string          `json:"id"`
	Epoch  string          `json:"epoch"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9-]{16,80}$`)

func Valid(r Request) bool {
	if !idPattern.MatchString(r.ID) || len(r.Body) > MaxBody {
		return false
	}
	switch r.Operation {
	case "status":
		return r.Method == "GET"
	case "settings", "settings-versioned", "presets":
		return r.Method == "GET" || r.Method == "PUT"
	case "preview":
		return r.Method == "POST" || r.Method == "DELETE"
	}
	return false
}

type record struct {
	fingerprint [32]byte
	reply       Reply
	at          time.Time
}
type Broker struct {
	mu      sync.Mutex
	epoch   string
	records map[string]record
	target  http.Handler
}

func New(target http.Handler) *Broker {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic(err)
	}
	return &Broker{epoch: hex.EncodeToString(nonce[:]), records: make(map[string]record), target: target}
}
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var input Request
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxWire+1))
	if err != nil || len(data) > MaxWire {
		http.Error(w, "panel request too large", 413)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || !Valid(input) {
		http.Error(w, "invalid panel request", 400)
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "trailing panel data", 400)
		return
	}
	reply := b.execute(input)
	_ = json.NewEncoder(w).Encode(reply)
}
func (b *Broker) execute(input Request) Reply {
	b.mu.Lock()
	defer b.mu.Unlock()
	errorReply := func(status int, message string) Reply {
		body, _ := json.Marshal(map[string]string{"error": message})
		return Reply{ID: input.ID, Epoch: b.epoch, Status: status, Body: body}
	}
	// A restarted receiver cannot replay earlier mutations. Force a reload
	// instead of silently creating another preview or re-saving stale data.
	if input.Epoch != b.epoch && !(input.Epoch == "" && input.Method == "GET") {
		return errorReply(409, "AirPlay restarted; reload the panel")
	}
	canonical, _ := json.Marshal(input)
	fingerprint := sha256.Sum256(canonical)
	if previous, ok := b.records[input.ID]; ok {
		if previous.fingerprint != fingerprint {
			return errorReply(409, "request ID reused with different data")
		}
		if time.Since(previous.at) < Retention {
			return previous.reply
		}
		// Expired duplicates never execute twice while still retained.
		return errorReply(410, "request expired; reload the panel")
	}
	for id, previous := range b.records {
		if time.Since(previous.at) >= Retention {
			delete(b.records, id)
		}
	}
	if len(b.records) >= Capacity {
		return errorReply(429, "panel is busy; retry later")
	}
	// Deliberately independent of the short-lived helper's request context.
	// Cancelling an invocation must not cancel a half-completed save.
	internal, _ := http.NewRequest(input.Method, "/"+input.Operation, bytes.NewReader(input.Body))
	internal.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	b.target.ServeHTTP(recorder, internal)
	reply := Reply{ID: input.ID, Epoch: b.epoch, Status: recorder.Code, Body: append(json.RawMessage(nil), recorder.Body.Bytes()...)}
	if !json.Valid(reply.Body) && recorder.Code >= 400 {
		reply = errorReply(recorder.Code, "panel backend rejected this request")
	} else if len(reply.Body) > MaxBody || !json.Valid(reply.Body) {
		reply = errorReply(502, "invalid panel backend response")
	}
	b.records[input.ID] = record{fingerprint: fingerprint, reply: reply, at: time.Now()}
	return reply
}
