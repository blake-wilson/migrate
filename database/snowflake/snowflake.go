package snowflake

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	nurl "net/url"
	"strconv"
	"strings"

	"go.uber.org/atomic"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/hashicorp/go-multierror"
	"github.com/lib/pq"
	sf "github.com/snowflakedb/gosnowflake"
)

func init() {
	db := Snowflake{}
	database.Register("snowflake", &db)
}

var DefaultMigrationsTable = "schema_migrations"

var (
	ErrNilConfig          = fmt.Errorf("no config")
	ErrNoDatabaseName     = fmt.Errorf("no database name")
	ErrNoPassword         = fmt.Errorf("no password")
	ErrNoSchema           = fmt.Errorf("no schema")
	ErrNoSchemaOrDatabase = fmt.Errorf("no schema/database name")
)

type Config struct {
	MigrationsTable string
	DatabaseName    string
}

type Snowflake struct {
	isLocked atomic.Bool
	conn     *sql.Conn
	db       *sql.DB

	// Open and WithInstance need to guarantee that config is never nil
	config *Config
}

func WithInstance(instance *sql.DB, config *Config) (database.Driver, error) {
	if config == nil {
		return nil, ErrNilConfig
	}

	if err := instance.Ping(); err != nil {
		fmt.Printf("failed to ping\n")
		return nil, err
	}

	if config.DatabaseName == "" {
		query := `SELECT CURRENT_DATABASE()`
		var databaseName string
		if err := instance.QueryRow(query).Scan(&databaseName); err != nil {
			return nil, &database.Error{OrigErr: err, Query: []byte(query)}
		}

		if len(databaseName) == 0 {
			return nil, ErrNoDatabaseName
		}

		config.DatabaseName = databaseName
	}

	if len(config.MigrationsTable) == 0 {
		config.MigrationsTable = DefaultMigrationsTable
	}

	conn, err := instance.Conn(context.Background())
	if err != nil {
		return nil, err
	}

	px := &Snowflake{
		conn:   conn,
		db:     instance,
		config: config,
	}

	if err := px.ensureVersionTable(); err != nil {
		return nil, err
	}

	return px, nil
}

func (p *Snowflake) Open(url string) (database.Driver, error) {
	purl, err := nurl.Parse(url)
	if err != nil {
		return nil, err
	}

	password, isPasswordSet := purl.User.Password()
	if !isPasswordSet {
		return nil, ErrNoPassword
	}
	fmt.Printf("Password: %s\n", password)

	splitPath := strings.Split(purl.Path, "/")
	if len(splitPath) < 3 {
		return nil, ErrNoSchemaOrDatabase
	}

	database := splitPath[2]
	if len(database) == 0 {
		return nil, ErrNoDatabaseName
	}

	schema := splitPath[1]
	if len(schema) == 0 {
		return nil, ErrNoSchema
	}

	// auth := purl.Query().Get("authentication")
	pKeyStr := strings.ReplaceAll(purl.Query().Get("privateKey"), `\n`, "\n")
	pKey, err := GetPrivateKey(pKeyStr)
	fmt.Printf("private key is %s\n\n", pKeyStr)
	if err != nil {
		return nil, err
	}
	role := purl.Query().Get("role")

	cfg := &sf.Config{
		Account: purl.Host,
		// Host:          purl.Host + ".snowflakecomputing.com",
		User:          purl.User.Username(),
		Authenticator: sf.AuthTypeJwt,
		Role:          role,
		PrivateKey:    pKey,
		// Password:      password,
		Database:  database,
		Schema:    schema,
		Warehouse: "SF_NATIVE_EXP_WH",
	}

	dsn, err := sf.DSN(cfg)
	if err != nil {
		return nil, err
	}

	fmt.Printf("database is %s\n", database)
	fmt.Printf("schema is %s\n", schema)
	fmt.Printf("user is %s\n", cfg.User)
	db, err := sql.Open("snowflake", dsn)
	if err != nil {
		fmt.Printf("open error\n\n")
		return nil, err
	}

	migrationsTable := purl.Query().Get("x-migrations-table")
	fmt.Printf("migrations table is %s\n", migrationsTable)

	px, err := WithInstance(db, &Config{
		DatabaseName:    database,
		MigrationsTable: migrationsTable,
	})
	if err != nil {
		fmt.Printf("Other error\n")
		return nil, err
	}

	return px, nil
}

func (p *Snowflake) Close() error {
	connErr := p.conn.Close()
	dbErr := p.db.Close()
	if connErr != nil || dbErr != nil {
		return fmt.Errorf("conn: %v, db: %v", connErr, dbErr)
	}
	return nil
}

func (p *Snowflake) Lock() error {
	if !p.isLocked.CAS(false, true) {
		return database.ErrLocked
	}
	return nil
}

func (p *Snowflake) Unlock() error {
	if !p.isLocked.CAS(true, false) {
		return database.ErrNotLocked
	}
	return nil
}

func (p *Snowflake) Run(migration io.Reader) error {
	migr, err := io.ReadAll(migration)
	if err != nil {
		return err
	}

	// run migration
	query := string(migr[:])
	p.conn.ExecContext(context.Background(), fmt.Sprintf("ALTER SESSION SET multi_statement_count=0"))
	if _, err := p.conn.ExecContext(context.Background(), query); err != nil {
		if pgErr, ok := err.(*pq.Error); ok {
			var line uint
			var col uint
			var lineColOK bool
			if pgErr.Position != "" {
				if pos, err := strconv.ParseUint(pgErr.Position, 10, 64); err == nil {
					line, col, lineColOK = computeLineFromPos(query, int(pos))
				}
			}
			message := fmt.Sprintf("migration failed: %s", pgErr.Message)
			if lineColOK {
				message = fmt.Sprintf("%s (column %d)", message, col)
			}
			if pgErr.Detail != "" {
				message = fmt.Sprintf("%s, %s", message, pgErr.Detail)
			}
			return database.Error{OrigErr: err, Err: message, Query: migr, Line: line}
		}
		return database.Error{OrigErr: err, Err: "migration failed", Query: migr}
	}

	return nil
}

func computeLineFromPos(s string, pos int) (line uint, col uint, ok bool) {
	// replace crlf with lf
	s = strings.Replace(s, "\r\n", "\n", -1)
	// pg docs: pos uses index 1 for the first character, and positions are measured in characters not bytes
	runes := []rune(s)
	if pos > len(runes) {
		return 0, 0, false
	}
	sel := runes[:pos]
	line = uint(runesCount(sel, newLine) + 1)
	col = uint(pos - 1 - runesLastIndex(sel, newLine))
	return line, col, true
}

const newLine = '\n'

func runesCount(input []rune, target rune) int {
	var count int
	for _, r := range input {
		if r == target {
			count++
		}
	}
	return count
}

func runesLastIndex(input []rune, target rune) int {
	for i := len(input) - 1; i >= 0; i-- {
		if input[i] == target {
			return i
		}
	}
	return -1
}

func (p *Snowflake) SetVersion(version int, dirty bool) error {
	tx, err := p.conn.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		return &database.Error{OrigErr: err, Err: "transaction start failed"}
	}

	query := `DELETE FROM "` + p.config.MigrationsTable + `"`
	if _, err := tx.Exec(query); err != nil {
		if errRollback := tx.Rollback(); errRollback != nil {
			err = multierror.Append(err, errRollback)
		}
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	// Also re-write the schema version for nil dirty versions to prevent
	// empty schema version for failed down migration on the first migration
	// See: https://github.com/golang-migrate/migrate/issues/330
	if version >= 0 || (version == database.NilVersion && dirty) {
		query = `INSERT INTO "` + p.config.MigrationsTable + `" (version,
				dirty) VALUES (` + strconv.FormatInt(int64(version), 10) + `,
				` + strconv.FormatBool(dirty) + `)`
		if _, err := tx.Exec(query); err != nil {
			if errRollback := tx.Rollback(); errRollback != nil {
				err = multierror.Append(err, errRollback)
			}
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}
	}

	if err := tx.Commit(); err != nil {
		return &database.Error{OrigErr: err, Err: "transaction commit failed"}
	}

	return nil
}

func (p *Snowflake) Version() (version int, dirty bool, err error) {
	query := `SELECT version, dirty FROM "` + p.config.MigrationsTable + `" LIMIT 1`
	err = p.conn.QueryRowContext(context.Background(), query).Scan(&version, &dirty)
	switch {
	case err == sql.ErrNoRows:
		return database.NilVersion, false, nil

	case err != nil:
		if e, ok := err.(*pq.Error); ok {
			if e.Code.Name() == "undefined_table" {
				return database.NilVersion, false, nil
			}
		}
		return 0, false, &database.Error{OrigErr: err, Query: []byte(query)}

	default:
		return version, dirty, nil
	}
}

func (p *Snowflake) Drop() (err error) {
	// select all tables in current schema
	query := `SELECT table_name FROM information_schema.tables WHERE table_schema=(SELECT current_schema()) AND table_type='BASE TABLE'`
	tables, err := p.conn.QueryContext(context.Background(), query)
	if err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	defer func() {
		if errClose := tables.Close(); errClose != nil {
			err = multierror.Append(err, errClose)
		}
	}()

	// delete one table after another
	tableNames := make([]string, 0)
	for tables.Next() {
		var tableName string
		if err := tables.Scan(&tableName); err != nil {
			return err
		}
		if len(tableName) > 0 {
			tableNames = append(tableNames, tableName)
		}
	}
	if err := tables.Err(); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	if len(tableNames) > 0 {
		// delete one by one ...
		for _, t := range tableNames {
			query = `DROP TABLE IF EXISTS ` + t + ` CASCADE`
			if _, err := p.conn.ExecContext(context.Background(), query); err != nil {
				return &database.Error{OrigErr: err, Query: []byte(query)}
			}
		}
	}

	return nil
}

// ensureVersionTable checks if versions table exists and, if not, creates it.
// Note that this function locks the database, which deviates from the usual
// convention of "caller locks" in the Snowflake type.
func (p *Snowflake) ensureVersionTable() (err error) {
	if err = p.Lock(); err != nil {
		return err
	}

	defer func() {
		if e := p.Unlock(); e != nil {
			if err == nil {
				err = e
			} else {
				err = multierror.Append(err, e)
			}
		}
	}()

	// check if migration table exists
	var count int
	query := `SELECT COUNT(1) FROM information_schema.tables WHERE table_name = $1 AND table_schema = (SELECT current_schema()) LIMIT 1`
	if err := p.conn.QueryRowContext(context.Background(), query, p.config.MigrationsTable).Scan(&count); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	if count == 1 {
		return nil
	}

	// if not, create the empty migration table
	query = `CREATE TABLE if not exists "` + p.config.MigrationsTable + `" (
			version bigint not null primary key, dirty boolean not null)`
	if _, err := p.conn.ExecContext(context.Background(), query); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	return nil
}

func GetPrivateKey(pKey string) (*rsa.PrivateKey, error) {
	privateKeyBlock, _ := pem.Decode([]byte(pKey))
	if privateKeyBlock == nil {
		return nil, errors.New("could not decode private key from config")
	}

	pk, err := x509.ParsePKCS8PrivateKey(privateKeyBlock.Bytes)
	if err != nil {
		return nil, err
	}

	var privKey *rsa.PrivateKey
	switch key := pk.(type) {
	case *rsa.PrivateKey:
		privKey = key
	default:
		return nil, errors.New("unable to parse private key")
	}

	return privKey, nil
}
