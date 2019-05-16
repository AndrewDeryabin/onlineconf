package common

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"gitlab.com/nyarla/go-crypt"
)

var authDB = openAuthDatabase()

func openAuthDatabase() *sql.DB {
	mysqlConfig := mysqlInitConfig(AdminConfig.Auth.DatabaseConfig)
	mysqlConfig.Params["allowOldPasswords"] = "1"
	db, err := sql.Open("mysql", mysqlConfig.FormatDSN())
	if err != nil {
		log.Fatal().Err(err).Msg("failed to open authentication database")
	}
	db.SetConnMaxLifetime(time.Duration(AdminConfig.Auth.DatabaseConfig.MaxLifetime) * time.Second)
	return db
}

func CheckUserPassword(ctx context.Context, user, password string) (bool, error) {
	query := fmt.Sprintf("SELECT `%s` FROM `%s` WHERE `%s` = ?", AdminConfig.Auth.PasswordField, AdminConfig.Auth.Table, AdminConfig.Auth.NameField)
	if AdminConfig.Auth.Condition != "" {
		query += " AND " + AdminConfig.Auth.Condition
	}
	row := authDB.QueryRowContext(ctx, query, user)
	var pwd string
	switch err := row.Scan(&pwd); err {
	case sql.ErrNoRows:
		return false, nil
	case nil:
		return crypt.Crypt(password, pwd) == pwd, nil
	default:
		return false, err
	}
}

var termRe = regexp.MustCompile("([%_])")

func SelectUsers(ctx context.Context, term string) ([]string, error) {
	order := "1"
	condition := make([]string, 0)
	bind := make([]interface{}, 0)
	if AdminConfig.Auth.Condition != "" {
		condition = append(condition, AdminConfig.Auth.Condition)
	}
	if term != "" {
		condition = append(condition, fmt.Sprintf("`%s` LIKE ?", AdminConfig.Auth.NameField))
		bind = append(bind, "%"+termRe.ReplaceAllString(term, "\\$1")+"%", term)
		order = fmt.Sprintf("INSTR(`%s`, ?), 1", AdminConfig.Auth.NameField)
	}
	query := fmt.Sprintf("SELECT `%s` FROM `%s`", AdminConfig.Auth.NameField, AdminConfig.Auth.Table)
	if len(condition) > 0 {
		query += " WHERE " + strings.Join(condition, " AND ")
	}
	query += " ORDER BY " + order
	rows, err := authDB.QueryContext(ctx, query, bind...)
	if err != nil {
		return nil, err
	}
	return ReadStrings(rows)
}
