package models

import "time"

// Intake target types.
const (
	IntakeTargetPortal    = "portal"
	IntakeTargetWorkspace = "workspace"
)

// Intake is the routing half of an email mailbox (WI-1644). The mailbox (a
// type='email' channel) owns the connection, credentials, and monitored
// address; an intake owns one folder and the target it feeds. One mailbox may
// feed several intakes, but only via distinct folders.
type Intake struct {
	ID         int    `json:"id"`
	MailboxID  int    `json:"mailbox_id"`
	Folder     string `json:"folder"`
	TargetType string `json:"target_type"` // portal | workspace
	TargetID   int    `json:"target_id"`
	// RequestTypeID is reserved for a future explicit request-type binding; a
	// portal intake resolves its system Email request type lazily, so the value
	// is not part of the API contract.
	RequestTypeID         *int      `json:"-"`
	ItemTypeID            *int      `json:"item_type_id,omitempty"` // workspace target
	RateLimitPerHour      *int      `json:"rate_limit_per_hour,omitempty"`
	ProcessingDisposition string    `json:"processing_disposition,omitempty"`
	Status                string    `json:"status"` // enabled | disabled
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`

	// Joined fields for API responses.
	MailboxName    string `json:"mailbox_name,omitempty"`
	MailboxAddress string `json:"mailbox_address,omitempty"`
	// Per-folder health. LastUID/UIDValidity come from email_intake_state;
	// LastPolledAt is that row's updated_at. RateLimitedCount counts tracking
	// rows this intake declined and an operator can requeue.
	LastUID          int        `json:"last_uid,omitempty"`
	UIDValidity      uint32     `json:"uid_validity,omitempty"`
	LastPolledAt     *time.Time `json:"last_polled_at,omitempty"`
	RateLimitedCount int        `json:"rate_limited_count,omitempty"`
}
