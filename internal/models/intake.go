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
	ID                    int       `json:"id"`
	MailboxID             int       `json:"mailbox_id"`
	Folder                string    `json:"folder"`
	TargetType            string    `json:"target_type"` // portal | workspace
	TargetID              int       `json:"target_id"`
	RequestTypeID         *int      `json:"request_type_id,omitempty"` // portal target
	ItemTypeID            *int      `json:"item_type_id,omitempty"`    // workspace target
	RateLimitPerHour      *int      `json:"rate_limit_per_hour,omitempty"`
	ProcessingDisposition string    `json:"processing_disposition,omitempty"`
	Status                string    `json:"status"` // enabled | disabled
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`

	// Joined fields for API responses.
	MailboxName    string `json:"mailbox_name,omitempty"`
	MailboxAddress string `json:"mailbox_address,omitempty"`
}
