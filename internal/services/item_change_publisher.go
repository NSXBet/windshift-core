package services

import "sync"

// ItemChangeKind labels the kind of item-detail mutation that should push a
// live update. The label is internal audit metadata only: the item event stream
// is deliberately coarse and emits a single `changed` event (or `deleted`) for
// every kind, so the client always reloads the whole detail and adding or
// removing a label cannot break it.
type ItemChangeKind string

const (
	ItemChangeCreated ItemChangeKind = "created"
	ItemChangeUpdated ItemChangeKind = "updated"
	ItemChangeStatus  ItemChangeKind = "status"
	ItemChangeDeleted ItemChangeKind = "deleted"
	ItemChangeComment ItemChangeKind = "comment"
	ItemChangeLink    ItemChangeKind = "link"
	ItemChangeZammad  ItemChangeKind = "zammad"
)

// ItemChangePublisher receives item-change notifications after a mutation has
// committed. Plan 2 (WI-484) supplies an in-memory SSE hub implementation; until
// then the process default is a no-op, so wiring publish calls into the mutation
// chokepoints changes no behavior and Plan 1 (WI-483) ships independently.
//
// Implementations must be safe for concurrent use: PublishItemChange is called
// from request goroutines, schedulers, and background workers.
type ItemChangePublisher interface {
	// PublishItemChange announces that the item identified by itemID changed. The
	// kind is recorded for diagnostics; subscribers see a coarse change. It must
	// be cheap and non-blocking; a hub implementation fans out to in-memory
	// subscribers without touching the database.
	PublishItemChange(itemID int, kind ItemChangeKind)
}

type noopItemChangePublisher struct{}

func (noopItemChangePublisher) PublishItemChange(int, ItemChangeKind) {}

var (
	itemChangePubMu sync.RWMutex
	itemChangePub   ItemChangePublisher = noopItemChangePublisher{}
)

// SetItemChangePublisher installs the process-wide item-change publisher. It is
// called once during server startup (Plan 2 passes the SSE hub) and may be
// swapped by tests. Passing nil restores the no-op default.
func SetItemChangePublisher(p ItemChangePublisher) {
	itemChangePubMu.Lock()
	defer itemChangePubMu.Unlock()
	if p == nil {
		p = noopItemChangePublisher{}
	}
	itemChangePub = p
}

// PublishItemChange routes an item-change notification to the installed
// publisher. It is the single entry point every mutation chokepoint calls, so
// coverage is auditable from one symbol.
//
// IMPORTANT: call this only AFTER the underlying database mutation has
// committed — never inside an open transaction that might still roll back. For
// destructive writes (delete), capture the item id (and any parent id) BEFORE
// the write and publish afterwards.
//
// itemID <= 0 is ignored, so callers can pass an optional parent id
// unconditionally.
func PublishItemChange(itemID int, kind ItemChangeKind) {
	if itemID <= 0 {
		return
	}
	itemChangePubMu.RLock()
	p := itemChangePub
	itemChangePubMu.RUnlock()
	p.PublishItemChange(itemID, kind)
}

// PublishItemDeletion preserves the deleted item's workspace for stream authorization.
func PublishItemDeletion(itemID, workspaceID int) {
	if itemID <= 0 {
		return
	}
	itemChangePubMu.RLock()
	p := itemChangePub
	itemChangePubMu.RUnlock()
	if publisher, ok := p.(interface{ PublishItemDeletion(int, int) }); ok {
		publisher.PublishItemDeletion(itemID, workspaceID)
		return
	}
	p.PublishItemChange(itemID, ItemChangeDeleted)
}
