package events

import "time"

// Event maps both [ts] columns writable: two violations (the second through
// GORM's naming of UpdatedAt).
type Event struct {
	ID        string    `gorm:"column:id"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time
}

func (Event) TableName() string { return "events" }

// Form gives created_at the write back with "<-:create": a violation;
// updated_at is read only: fine.
type Form struct {
	ID        string    `gorm:"column:id"`
	CreatedAt time.Time `gorm:"column:created_at;->;<-:create"`
	UpdatedAt time.Time `gorm:"column:updated_at;->"`
}

func (*Form) TableName() string { return "event_forms" }

// Upsert writes updated_at on UPDATE: a violation.
type Upsert struct {
	ID        string    `gorm:"column:id"`
	UpdatedAt time.Time `gorm:"column:updated_at;<-:update"`
}

func (Upsert) TableName() string { return "event_forms" }

// Staff maps a [c] table: fine.
type Staff struct {
	ID        string    `gorm:"column:id"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (Staff) TableName() string { return "event_staff" }

// Post is the rule (read only, "<-:false" too): fine.
type Post struct {
	ID        string    `gorm:"column:id"`
	CreatedAt time.Time `gorm:"column:created_at;->"`
	UpdatedAt time.Time `gorm:"column:updated_at;<-:false"`
}

func (Post) TableName() string { return "posts" }

// row is a [ts] row type without TableName (Table() or Raw): its writable
// created_at is a violation.
type row struct {
	ID        string    `gorm:"column:id"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;->"`
}

// view has no gorm tag and no TableName: not a model, fine.
type view struct {
	CreatedAt, UpdatedAt time.Time
}

// staffRow maps created_at alone without TableName: not a [ts] row type.
type staffRow struct {
	ID        string    `gorm:"column:id"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

const (
	insertPost  = `INSERT INTO posts (id, created_at, updated_at) VALUES ( ?, ?, ? )` // violation
	insertEvent = `INSERT INTO events (id, org_id) VALUES ( ?, ? )`                   // fine: the defaults stamp it
	insertStaff = `INSERT INTO event_staff (id, created_at) VALUES ( ?, ? )`          // [c]: fine
	formCols    = "id, "
	insertForm  = "INSERT INTO public.event_forms (" + formCols + "updated_at) VALUES ( ?, ? )" // violation across a concatenation
)
