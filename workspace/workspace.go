// Package workspace provides functions for interacting with a temporary MySQL
// schema. It manages creating a schema on a desired location (either an
// existing MySQL instance, or a dynamically-controlled Docker instance),
// running SQL DDL or DML, introspecting the resulting schema, and cleaning
// up the schema when it is no longer needed.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	log "github.com/sirupsen/logrus"
	"github.com/skeema/skeema/fs"
	"github.com/skeema/tengo"
)

// Workspace represents a "scratch space" for DDL operations and schema
// introspection.
type Workspace interface {
	// ConnectionPool returns a *sqlx.DB representing a connection pool for
	// interacting with the workspace. The pool should already be using the
	// correct default database for interacting with the workspace schema.
	ConnectionPool(params string) (*sqlx.DB, error)

	// IntrospectSchema returns a *tengo.Schema representing the current state
	// of the workspace schema.
	IntrospectSchema() (*tengo.Schema, error)

	// Cleanup cleans up the workspace, leaving it in a state where it could be
	// re-used/re-initialized as needed. Repeated calls to Cleanup() may error.
	Cleanup() error
}

// Type represents a kind of workspace to use.
type Type int

// Constants enumerating different types of workspaces
const (
	TypeTempSchema  Type = iota // A temporary schema on a real pre-supplied Instance
	TypeLocalDocker             // A schema on an ephemeral Docker container on localhost
	TypePrefab                  // A pre-supplied Workspace, possibly from another package
)

// CleanupAction represents how to clean up a workspace.
type CleanupAction int

// Constants enumerating different cleanup actions. These may affect the
// behavior of Workspace.Cleanup() and/or Shutdown().
const (
	// CleanupActionNone means to perform no special cleanup
	CleanupActionNone CleanupAction = iota

	// CleanupActionDrop means to drop the schema in Workspace.Cleanup(). Only
	// used with TypeTempSchema.
	CleanupActionDrop

	// CleanupActionStop means to stop the MySQL instance container in Shutdown().
	// Only used with TypeLocalDocker.
	CleanupActionStop

	// CleanupActionDestroy means to destroy the MySQL instance container in
	// Shutdown(). Only used with TypeLocalDocker.
	CleanupActionDestroy
)

// Options represent different parameters controlling the workspace that is
// used. Some options are specific to a Type.
type Options struct {
	Type                Type
	CleanupAction       CleanupAction
	Instance            *tengo.Instance // only TypeTempSchema
	Flavor              tengo.Flavor    // only TypeLocalDocker
	ContainerName       string          // only TypeLocalDocker
	SchemaName          string
	DefaultCharacterSet string
	DefaultCollation    string
	DefaultConnParams   string    // only TypeLocalDocker
	RootPassword        string    // only TypeLocalDocker
	PrefabWorkspace     Workspace // only TypePrefab
	LockWaitTimeout     time.Duration
}

// New returns a pointer to a ready-to-use Workspace, using the configuration
// specified in opts.
func New(opts Options) (Workspace, error) {
	switch opts.Type {
	case TypeTempSchema:
		return NewTempSchema(opts)
	case TypeLocalDocker:
		return NewLocalDocker(opts)
	case TypePrefab:
		return opts.PrefabWorkspace, nil
	}
	return nil, fmt.Errorf("Unsupported workspace type %v", opts.Type)
}

// OptionsForDir returns Options based on the configuration in an fs.Dir.
// A non-nil instance should be supplied, unless the caller already knows the
// workspace won't be temp-schema based.
// This method relies on option definitions from util.AddGlobalOptions(),
// including "workspace", "temp-schema", "flavor", "docker-cleanup", and
// "reuse-temp-schema".
func OptionsForDir(dir *fs.Dir, instance *tengo.Instance) (Options, error) {
	requestedType, err := dir.Config.GetEnum("workspace", "temp-schema", "docker")
	if err != nil {
		return Options{}, err
	}
	opts := Options{
		CleanupAction:   CleanupActionNone,
		SchemaName:      dir.Config.Get("temp-schema"),
		LockWaitTimeout: 30 * time.Second,
	}
	if requestedType == "docker" {
		opts.Type = TypeLocalDocker
		opts.Flavor = tengo.NewFlavor(dir.Config.Get("flavor"))
		if !opts.Flavor.Known() && instance != nil {
			opts.Flavor = instance.Flavor()
		}
		opts.ContainerName = fmt.Sprintf("skeema-%s", strings.Replace(opts.Flavor.String(), ":", "-", -1))
		if cleanup, err := dir.Config.GetEnum("docker-cleanup", "none", "stop", "destroy"); err != nil {
			return Options{}, err
		} else if cleanup == "stop" {
			opts.CleanupAction = CleanupActionStop
		} else if cleanup == "destroy" {
			opts.CleanupAction = CleanupActionDestroy
		}
		if opts.DefaultConnParams, err = dir.InstanceDefaultParams(); err != nil {
			return Options{}, err
		}
	} else {
		opts.Type = TypeTempSchema
		opts.Instance = instance
		if !dir.Config.GetBool("reuse-temp-schema") {
			opts.CleanupAction = CleanupActionDrop
		}
		// Note: no support for opts.DefaultConnParams for temp-schema because the
		// supplied instance already has default params
	}
	return opts, nil
}

// ShutdownFunc is a function that manages final cleanup of a Workspace upon
// completion of a request or process. It may optionally use args, passed
// through by Shutdown(), to determine whether or not a Workspace needs to be
// cleaned up. It should return true if cleanup occurred (meaning that the
// ShutdownFunc should be de-registered from future calls to Shutdown()), or
// false otherwise.
type ShutdownFunc func(...interface{}) bool

var shutdownFuncs []ShutdownFunc
var shutdownFuncMutex sync.Mutex

// Shutdown performs any necessary cleanup operations prior to the program
// exiting. For example, if containers need to be stopped or destroyed, it is
// most efficient to do so at program exit, rather than needlessly doing so
// for each workspace invocation.
// It is recommended that programs importing this package call Shutdown as a
// deferred function in main().
func Shutdown(args ...interface{}) {
	shutdownFuncMutex.Lock()
	defer shutdownFuncMutex.Unlock()
	retainedFuncs := make([]ShutdownFunc, 0, len(shutdownFuncs))
	for _, f := range shutdownFuncs {
		if deregister := f(args...); !deregister {
			retainedFuncs = append(retainedFuncs, f)
		}
	}
	shutdownFuncs = retainedFuncs
}

// RegisterShutdownFunc registers a function to be executed by Shutdown.
// Structs satisfying the Workspace interface may optionally use this function
// to track actions to perform at shutdown time, such as stopping or destroying
// containers.
func RegisterShutdownFunc(f ShutdownFunc) {
	shutdownFuncMutex.Lock()
	defer shutdownFuncMutex.Unlock()
	shutdownFuncs = append(shutdownFuncs, f)
}

// StatementError represents a problem that occurred when executing a specific
// fs.Statement in a Workspace.
type StatementError struct {
	*fs.Statement
	Err error
}

// Error satisfies the builtin error interface.
func (se *StatementError) Error() string {
	loc := se.Location()
	if loc == "" {
		return fmt.Sprintf("%s [Full SQL: %s]", se.Err.Error(), se.Body())
	}
	return fmt.Sprintf("%s: %s", loc, se.Err.Error())
}

func (se *StatementError) String() string {
	return se.Error()
}

// Schema captures the result of executing the SQL from an fs.LogicalSchema
// in a workspace, and then introspecting the resulting schema. It wraps the
// introspected tengo.Schema alongside the original fs.LogicalSchema and any
// SQL errors that occurred.
type Schema struct {
	*tengo.Schema
	LogicalSchema *fs.LogicalSchema
	Failures      []*StatementError
}

// CopyWithName makes a copy of a workspace.Schema and assigns a different
// schema name. The copy is a shallow copy in terms of most fields: it points
// to the same LogicalSchema, Failures, and all pointer/reference fields
// inside the tengo.Schema. The tengo.Schema itself is a copy though, allowing
// the name to be changed without affecting the receiver.
func (wsSchema *Schema) CopyWithName(schemaName string) *Schema {
	schemaCopy := *wsSchema.Schema
	schemaCopy.Name = schemaName
	return &Schema{
		Schema:        &schemaCopy,
		LogicalSchema: wsSchema.LogicalSchema,
		Failures:      wsSchema.Failures,
	}
}

// FailedKeys returns a slice of tengo.ObjectKey values corresponding to
// statements that had SQL errors when executed.
func (wsSchema *Schema) FailedKeys() (result []tengo.ObjectKey) {
	for _, statementError := range wsSchema.Failures {
		result = append(result, statementError.ObjectKey())
	}
	return result
}

// ExecLogicalSchema converts a LogicalSchema into a workspace.Schema. It
// obtains a Workspace, executes the creation DDL contained in a LogicalSchema
// there, introspects it into a *tengo.Schema, cleans up the Workspace, and then
// returns a value containing the introspected schema and any SQL errors (e.g.
// tables that could not be created). Such individual statement errors are not
// fatal and are not included in the error return value. The error return value
// only represents fatal errors that prevented the entire process.
func ExecLogicalSchema(logicalSchema *fs.LogicalSchema, opts Options) (wsSchema *Schema, fatalErr error) {
	if logicalSchema.CharSet != "" {
		opts.DefaultCharacterSet = logicalSchema.CharSet
	}
	if logicalSchema.Collation != "" {
		opts.DefaultCollation = logicalSchema.Collation
	}
	var ws Workspace
	ws, fatalErr = New(opts)
	if fatalErr != nil {
		return
	}
	defer func() {
		if cleanupErr := ws.Cleanup(); fatalErr == nil {
			fatalErr = cleanupErr
		}
	}()

	// We need two separate connection pools: one with normal session settings,
	// and another that removes the Skeema-specific sql_mode override. The latter
	// is needed for object types that "remember" their creation-time sql_mode.
	db, err := ws.ConnectionPool("")
	if err != nil {
		fatalErr = fmt.Errorf("Cannot connect to workspace: %s", err)
		return
	}
	dbRemember, err := ws.ConnectionPool("sql_mode=@@GLOBAL.sql_mode")
	if err != nil {
		fatalErr = fmt.Errorf("Cannot connect to workspace: %s", err)
		return
	}
	rememberSQLMode := map[tengo.ObjectType]bool{
		tengo.ObjectTypeFunc: true,
		tengo.ObjectTypeProc: true,
		//tengo.ObjectTypeEvent: true,   // not implemented yet
		//tengo.ObjectTypeTrigger: true, // not implemented yet
	}

	// Run all CREATEs in parallel. Temporarily limit max open conns as a simple
	// means of limiting concurrency.
	defer db.SetMaxOpenConns(0)
	defer dbRemember.SetMaxOpenConns(0)
	db.SetMaxOpenConns(10)
	dbRemember.SetMaxOpenConns(10)
	results := make(chan *StatementError)
	for _, stmt := range logicalSchema.Creates {
		go func(statement *fs.Statement) {
			if rememberSQLMode[statement.ObjectType] {
				results <- execStatement(dbRemember, statement)
			} else {
				results <- execStatement(db, statement)
			}
		}(stmt)
	}
	wsSchema = &Schema{
		LogicalSchema: logicalSchema,
		Failures:      []*StatementError{},
	}
	for range logicalSchema.Creates {
		if result := <-results; result != nil {
			wsSchema.Failures = append(wsSchema.Failures, result)
		}
	}
	close(results)

	// Run ALTERs sequentially, since foreign key manipulations don't play
	// nice with concurrency.
	for _, statement := range logicalSchema.Alters {
		if err := execStatement(db, statement); err != nil {
			wsSchema.Failures = append(wsSchema.Failures, err)
		}
	}

	wsSchema.Schema, fatalErr = ws.IntrospectSchema()
	return
}

func execStatement(db *sqlx.DB, statement *fs.Statement) (stmtErr *StatementError) {
	_, err := db.Exec(statement.Body())
	if err == nil {
		return nil
	}
	stmtErr = &StatementError{
		Statement: statement,
		Err:       fmt.Errorf("Error executing DDL in workspace: %s", err),
	}
	if tengo.IsSyntaxError(err) {
		stmtErr.Err = fmt.Errorf("SQL syntax error: %s", err)
	}
	return stmtErr
}

// releaseFunc is a function to release a lock obtained by getLock
type releaseFunc func()

func getLock(instance *tengo.Instance, lockName string, maxWait time.Duration) (releaseFunc, error) {
	db, err := instance.Connect("", "")
	if err != nil {
		return nil, err
	}
	lockConn, err := db.Conn(context.Background())
	if err != nil {
		return nil, err
	}

	done := make(chan struct{})
	release := func() {
		close(done)
	}
	connMaintainer := func() {
		var result int
		defer lockConn.Close()
		for {
			select {
			case <-done:
				err := lockConn.QueryRowContext(context.Background(), "SELECT RELEASE_LOCK(?)", lockName).Scan(&result)
				if err != nil || result != 1 {
					log.Warnf("%s: Failed to release lock, or lock released early due to connection being dropped: %s [%d]", instance, err, result)
				}
				return
			case <-time.After(750 * time.Millisecond):
				err := lockConn.QueryRowContext(context.Background(), "SELECT 1").Scan(&result)
				if err != nil {
					log.Warnf("%s: Lock released early due to connection being dropped: %s", instance, err)
					return
				}
			}
		}
	}

	var getLockResult int
	start := time.Now()
	for time.Since(start) < maxWait {
		// Only using a timeout of 1 sec on each query to avoid potential issues with
		// query killers, spurious slow query logging, etc
		err := lockConn.QueryRowContext(context.Background(), "SELECT GET_LOCK(?, 1)", lockName).Scan(&getLockResult)
		if err == nil && getLockResult == 1 {
			// Launch a goroutine to keep the connection active, and release the lock
			// once the ReleaseFunc is called
			go connMaintainer()
			return release, nil
		}
	}
	return nil, errors.New("Unable to acquire lock")
}
