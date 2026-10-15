package store

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/ariesworx/starfix/internal/logs"
)

// driverLog is the database driver's logger over the store's. The
// driver speaks when a connection to Dolt turns out to be dead (`closing
// bad idle connection`, a broken pipe): the pool then dials again, so it
// is a warning, and what it says is the driver's text, in one field.
type driverLog struct{ log *slog.Logger }

// Print implements mysql.Logger.
func (d driverLog) Print(v ...any) {
	d.log.Warn("the database driver reported a connection to Dolt", "event", logs.DoltDriver,
		logs.KeyDetail, strings.TrimSpace(fmt.Sprint(v...)))
}
