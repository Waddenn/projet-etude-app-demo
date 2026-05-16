package model

import "time"

type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityMedium Priority = "medium"
	PriorityHigh   Priority = "high"
)

func ValidPriority(p string) bool {
	switch Priority(p) {
	case PriorityLow, PriorityMedium, PriorityHigh:
		return true
	}
	return false
}

type Status string

const (
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

type Ticket struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Priority    Priority  `json:"priority"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type JobKind string

const (
	JobKindWebhookNotify JobKind = "webhook.notify"
)

type AuditResult string

const (
	AuditOK     AuditResult = "ok"
	AuditDenied AuditResult = "denied"
	AuditError  AuditResult = "error"
)

type AuditEntry struct {
	ID           int64       `json:"id,omitempty"`
	Timestamp    time.Time   `json:"ts,omitempty"`
	UserSub      string      `json:"user_sub"`
	UserEmail    string      `json:"user_email,omitempty"`
	Action       string      `json:"action"`
	ResourceType string      `json:"resource_type,omitempty"`
	ResourceID   string      `json:"resource_id,omitempty"`
	IP           string      `json:"ip,omitempty"`
	UserAgent    string      `json:"user_agent,omitempty"`
	TraceID      string      `json:"trace_id,omitempty"`
	Result       AuditResult `json:"result"`
}

type Job struct {
	ID           int64
	Kind         JobKind
	Payload      []byte
	Attempts     int
	MaxAttempts  int
	TraceContext []byte // JSON map[string]string, sérialisé via OTel propagator.
}
