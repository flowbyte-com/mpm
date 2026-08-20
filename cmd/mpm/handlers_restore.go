package main

import (
	"database/sql"
	"fmt"
	"github.com/flowbyte-com/mpm-core/usererror"
)

func handleRestore(args []string) int {
	if len(args) < 2 {
		return respond("", "Usage: mpm restore <memory-id>\n", 1)
	}
	id := args[1]

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Fetch current state to preserve is_long_term and avoid overwriting weight
	var currentIsLTM int
	var currentWeight float64
	var err error
	err = dm.SQLDB().QueryRow(
		`SELECT COALESCE(is_long_term, 0), COALESCE(weight, 1) FROM memories WHERE id = ?`,
		id,
	).Scan(&currentIsLTM, &currentWeight)
	if err != nil {
		if err == sql.ErrNoRows {
			return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
		}
		usererror.Error("%v", err)
	}

	// Restore deleted_at, reset weight to max(original weight, 1) to prevent 0-weight limbo.
	// LTM status is preserved.
	result, err := dm.SQLDB().Exec(
		`UPDATE memories SET deleted_at = NULL, weight = MAX(?, 1) WHERE id = ?`,
		currentWeight, id,
	)
	if err != nil {
		usererror.Error("%v", err)
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
	}
	status := "restored"
	if currentIsLTM == 1 {
		status = "restored (LTM preserved)"
	}
	fmt.Printf("%s: %s\n", status, id)
	return 0
}
