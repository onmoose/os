package store

import (
	"database/sql"
	"time"
)

// madeFoldersDDL is the table of top-level folders the brain made in a user's
// home through moose (STORAGE.md # Folders made through moose, #655). Only a
// folder with a row, a use-case folder, or a folder inside one of those can be
// given to an app (APP_ISOLATION.md # User content). The name is the folder's
// name at the top of the home, such as "Recipes". A row stays when no app uses
// the folder any more: the folder belongs to the user. It goes with the user.
const madeFoldersDDL = `
CREATE TABLE IF NOT EXISTS made_folders (
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, name)
);`

// AddMadeFolder records that the brain made the top-level folder name in the
// user's home. Adding a row that is already there changes nothing, so an
// install that is tried again after a failure does not fail here.
func (s *Store) AddMadeFolder(userID, name string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO made_folders (user_id, name, created_at) VALUES (?, ?, ?)`,
		userID, name, time.Now().Unix())
	return err
}

// madeFolder is one made_folders row, as a failed user delete keeps it to put
// it back (DeletedUser).
type madeFolder struct {
	userID, name string
	createdAt    int64
}

// listMadeFolders reads the user's rows inside tx, for DeleteUserAndAccountValues.
func listMadeFolders(tx *sql.Tx, userID string) ([]madeFolder, error) {
	rows, err := tx.Query(`SELECT name, created_at FROM made_folders WHERE user_id=? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []madeFolder
	for rows.Next() {
		f := madeFolder{userID: userID}
		if err := rows.Scan(&f.name, &f.createdAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// MadeFolders returns the names of the top-level folders the brain made for
// the user, sorted by name. A user with none gets an empty list.
func (s *Store) MadeFolders(userID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM made_folders WHERE user_id=? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
