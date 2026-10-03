package events

// rawSQL breaks the GORM parameter rule of 06 H-30 three times.
func rawSQL(tx gormDB, id, org any) {
	tx.Exec(`INSERT INTO events (id, org_id) VALUES (?, ?)`, id, org)   // violation: "(?"
	tx.Raw("SELECT * FROM events WHERE id IN "+"(@ids)", id)            // violation: "(@" across a concatenation
	tx.Where("(? = id)", id)                                            // violation
	tx.Exec(`INSERT INTO events (id, org_id) VALUES ( ?, ? )`, id, org) // fine
	tx.Where("id IN ?", id).Where("org_id = ANY( ? )", org)             // fine
	re.MustCompile(`(?i)select`)                                        // not GORM: fine
}
