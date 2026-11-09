package sqldialect

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// mysqlConfig parses a go-sql-driver DSN (user:pass@tcp(host:3306)/db) and
// forces the options the shared queries depend on: rows-matched (not
// rows-changed) counts, utf8mb4 with binary collation, no LOAD DATA LOCAL.
func mysqlConfig(dsn string) (*mysql.Config, error) {
	c, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	if c.DBName == "" {
		return nil, fmt.Errorf("mysql DSN names no database")
	}
	c.AllowAllFiles = false
	c.ClientFoundRows = true
	c.ParseTime = true
	c.MultiStatements = false
	c.Loc = time.UTC
	if err := c.Apply(mysql.Charset("utf8mb4", "utf8mb4_bin")); err != nil {
		return nil, err
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 60 * time.Second
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 60 * time.Second
	}
	return c, nil
}

// OpenMySQL opens a pool on a go-sql-driver DSN whose connections run with
// the session settings the shared queries need.
func OpenMySQL(dsn string) (*sql.DB, error) {
	c, err := mysqlConfig(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := mysql.NewConnector(c)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(sessionConnector{conn}), nil
}

// sessionConnector runs mysqlSession on every new connection.
type sessionConnector struct{ driver.Connector }

func (c sessionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	ex, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("mysql connection cannot exec")
	}
	for _, q := range mysqlSession {
		if _, err := ex.ExecContext(ctx, q, nil); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("mysql session setup: %w", err)
		}
	}
	return conn, nil
}
