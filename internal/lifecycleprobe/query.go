package lifecycleprobe

import (
	"context"
	"database/sql"
)

func DeleteUser(ctx context.Context, database *sql.DB, username string) error {
	_, err := database.ExecContext(ctx, "DELETE FROM users WHERE username = '"+username+"'")
	return err
}
