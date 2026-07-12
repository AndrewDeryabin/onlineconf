package admin

import (
	"database/sql"
	"errors"
	"regexp"

	"github.com/go-sql-driver/mysql"
	"github.com/rs/zerolog/log"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

type AdminConfig struct {
	Auth                 AuthenticatorConfig
	NotificationDatabase DatabaseConfig `yaml:"notification_database"`
}

var adminConfig *AdminConfig

func Initialize(config AdminConfig) {
	adminConfig = &config
	initAuthenticator(config.Auth)
	if config.NotificationDatabase.Host != "" || config.NotificationDatabase.Base != "" {
		notifyDB = OpenDatabase(config.NotificationDatabase)
	}
	ensureSchemaCurrent()
	initializeDeps()
}

// ensureSchemaCurrent aborts startup with an actionable message when the
// database misses a column this version reads unconditionally — the upgrade
// mistake that otherwise surfaces as an endless stream of unknown-column
// errors on every request. Connectivity problems are deliberately not judged
// here: the deps initialization retries those.
func ensureSchemaCurrent() {
	row := DB.QueryRow("SELECT Templated FROM my_config_tree LIMIT 1")
	var templated bool
	err := row.Scan(&templated)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return
	}
	if isUnknownColumn(err) {
		log.Fatal().Err(err).Msg("the database schema is behind this version: apply admin/etc/update-9.sql (and any earlier update-*.sql not applied yet)")
	}
	log.Warn().Err(err).Msg("schema currency check skipped")
}

// isUnknownColumn reports MySQL error 1054 (unknown column), wrapped or not.
func isUnknownColumn(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1054
}

var pathRe = regexp.MustCompile(`^(.*)/([^/]+)$`)

func splitPath(path string) (string, string, error) {
	m := pathRe.FindStringSubmatch(path)
	if m == nil {
		return "", "", ErrInvalidPath
	}
	parentPath, name := m[1], m[2]
	if parentPath == "" {
		parentPath = "/"
	}
	return parentPath, name, nil
}

var likeRe = regexp.MustCompile("([%_])")

func likeEscape(str string) string {
	return likeRe.ReplaceAllString(str, `\$1`)
}
