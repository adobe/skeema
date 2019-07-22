package dumper

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/skeema/mybase"
	"github.com/skeema/skeema/fs"
	"github.com/skeema/skeema/util"
	"github.com/skeema/skeema/workspace"
	"github.com/skeema/tengo"
)

func TestMain(m *testing.M) {
	// Suppress packet error output when attempting to connect to a Dockerized
	// mysql-server which is still starting up
	tengo.UseFilteredDriverLogger()

	os.Exit(m.Run())
}

func TestOptionsIgnore(t *testing.T) {
	var opts Options
	assertIgnore := func(ot tengo.ObjectType, name string, expected bool) {
		t.Helper()
		key := tengo.ObjectKey{Type: ot, Name: name}
		if actual := opts.shouldIgnore(key); actual != expected {
			t.Errorf("Unexpected result from shouldIgnore(%s): expected %t, found %t", key, expected, actual)
		}
	}

	// Confirm behavior of IgnoreTable
	opts = Options{
		IgnoreTable: regexp.MustCompile("^multi"),
	}
	assertIgnore(tengo.ObjectTypeTable, "multi1", true)
	assertIgnore(tengo.ObjectTypeTable, "ultimulti", false)
	assertIgnore(tengo.ObjectTypeFunc, "multi1", false)

	// Confirm behavior of OnlyKeys
	keys := []tengo.ObjectKey{
		{Type: tengo.ObjectTypeTable, Name: "cats"},
		{Type: tengo.ObjectTypeTable, Name: "tigers"},
		{Type: tengo.ObjectTypeProc, Name: "pounce"},
	}
	opts = Options{}
	opts.OnlyKeys(keys)
	assertIgnore(tengo.ObjectTypeTable, "multi1", true)
	assertIgnore(tengo.ObjectTypeTable, "cats", false)
	assertIgnore(tengo.ObjectTypeFunc, "pounce", true)

	// Confirm behavior of IgnoreKeys
	opts = Options{}
	opts.IgnoreKeys(keys)
	assertIgnore(tengo.ObjectTypeTable, "multi1", false)
	assertIgnore(tengo.ObjectTypeTable, "cats", true)
	assertIgnore(tengo.ObjectTypeFunc, "pounce", false)

	// Confirm behavior of combination of these settings
	opts = Options{
		IgnoreTable: regexp.MustCompile("^multi"),
	}
	opts.IgnoreKeys(keys)
	opts.OnlyKeys([]tengo.ObjectKey{
		{Type: tengo.ObjectTypeTable, Name: "cats"},
		{Type: tengo.ObjectTypeTable, Name: "dogs"},
	})
	assertIgnore(tengo.ObjectTypeTable, "multi1", true)
	assertIgnore(tengo.ObjectTypeTable, "cats", true)
	assertIgnore(tengo.ObjectTypeTable, "horses", true)
	assertIgnore(tengo.ObjectTypeTable, "dogs", false)
}

func TestIntegration(t *testing.T) {
	images := tengo.SplitEnv("SKEEMA_TEST_IMAGES")
	if len(images) == 0 {
		fmt.Println("SKEEMA_TEST_IMAGES env var is not set, so integration tests will be skipped!")
		fmt.Println("To run integration tests, you may set SKEEMA_TEST_IMAGES to a comma-separated")
		fmt.Println("list of Docker images. Example:\n# SKEEMA_TEST_IMAGES=\"mysql:5.6,mysql:5.7\" go test")
	}
	manager, err := tengo.NewDockerClient(tengo.DockerClientOptions{})
	if err != nil {
		t.Errorf("Unable to create sandbox manager: %s", err)
	}
	suite := &IntegrationSuite{manager: manager}
	tengo.RunSuite(suite, t, images)
}

type IntegrationSuite struct {
	manager         *tengo.DockerClient
	d               *tengo.DockerizedInstance
	schema          *tengo.Schema
	scratchDir      *fs.Dir
	statementErrors []*workspace.StatementError
}

// TestFormatSimple tests simple reformatting, where the filesystem and schema
// match aside from formatting differences and statement errors. This is similar
// to the usage pattern of `skeema format` or `skeema lint --format`.
func (s IntegrationSuite) TestFormatSimple(t *testing.T) {
	opts := Options{
		IncludeAutoInc: true,
		CountOnly:      true,
	}
	if len(s.statementErrors) != 1 {
		t.Fatalf("Expected one StatementError from test setup; found %d", len(s.statementErrors))
	}
	opts.IgnoreKeys([]tengo.ObjectKey{s.statementErrors[0].ObjectKey()})
	count, err := DumpSchema(s.schema, s.scratchDir, opts)
	expected := len(s.scratchDir.LogicalSchemas[0].Creates) - 2 // no reformat needed for table fine, plus one statementerror
	if count != expected || err != nil {
		t.Errorf("Expected FormatLogicalSchema() to return (%d, nil); instead found (%d, %v)", expected, count, err)
	}

	// Since above run enabled opts.CountOnly, repeated run with it disabled
	// should return the same count, and another run after that should return 0 count
	opts.CountOnly = false
	count, err = DumpSchema(s.schema, s.scratchDir, opts)
	if count != expected || err != nil {
		t.Errorf("Expected FormatLogicalSchema() to return (%d, nil); instead found (%d, %v)", expected, count, err)
	}
	count, err = DumpSchema(s.schema, s.scratchDir, opts)
	if expected = 0; count != expected || err != nil {
		t.Errorf("Expected FormatLogicalSchema() to return (%d, nil); instead found (%d, %v)", expected, count, err)
	}
	s.verifyFormat(t)
}

// TestFormatPull tests a use-case closer to `skeema pull`, where in addition
// to files being reformatted, there are also objects that only exist in the
// filesystem or only exist in the database.
func (s IntegrationSuite) TestFormatPull(t *testing.T) {
	opts := Options{
		IncludeAutoInc: true,
		CountOnly:      true,
	}
	if len(s.statementErrors) != 1 {
		t.Fatalf("Expected one StatementError from test setup; found %d", len(s.statementErrors))
	}
	opts.IgnoreKeys([]tengo.ObjectKey{s.statementErrors[0].ObjectKey()})

	// In the fs, rename posts table and its file. Expectation is that
	// FormatLogicalSchema will undo this action.
	contents := fs.ReadTestFile(t, s.testdata(".scratch", "posts.sql"))
	contents = strings.Replace(contents, "CREATE TABLE posts", "create table widgets", 1)
	fs.WriteTestFile(t, s.testdata(".scratch", "widgets.sql"), contents)
	fs.RemoveTestFile(t, s.testdata(".scratch", "posts.sql"))
	s.reparseScratchDir(t)

	count, err := DumpSchema(s.schema, s.scratchDir, opts)
	expected := len(s.scratchDir.LogicalSchemas[0].Creates) - 1 // no reformat needed for fine.sql or invalid.sql, but 1 extra from above manipulations
	if count != expected || err != nil {
		t.Errorf("Expected FormatLogicalSchema() to return (%d, nil); instead found (%d, %v)", expected, count, err)
	}

	// Since above run enabled opts.CountOnly, repeated run with it disabled
	// should return the same count, and another run after that should return 0 count
	opts.CountOnly = false
	count, err = DumpSchema(s.schema, s.scratchDir, opts)
	if count != expected || err != nil {
		t.Errorf("Expected FormatLogicalSchema() to return (%d, nil); instead found (%d, %v)", expected, count, err)
	}
	s.reparseScratchDir(t)
	count, err = DumpSchema(s.schema, s.scratchDir, opts)
	if expected = 0; count != expected || err != nil {
		t.Errorf("Expected FormatLogicalSchema() to return (%d, nil); instead found (%d, %v)", expected, count, err)
	}
	s.verifyFormat(t)
}

func (s *IntegrationSuite) Setup(backend string) (err error) {
	s.d, err = s.manager.GetOrCreateInstance(tengo.DockerizedInstanceOptions{
		Name:         fmt.Sprintf("skeema-test-%s", strings.Replace(backend, ":", "-", -1)),
		Image:        backend,
		RootPassword: "fakepw",
	})
	return err
}

func (s *IntegrationSuite) Teardown(backend string) error {
	if err := s.d.Stop(); err != nil {
		return err
	}
	return os.RemoveAll(s.scratchPath())
}

func (s *IntegrationSuite) BeforeTest(method string, backend string) error {
	if err := s.d.NukeData(); err != nil {
		return err
	}
	if _, err := os.Stat(s.scratchPath()); err == nil { // dir exists
		if err := os.RemoveAll(s.scratchPath()); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(s.scratchPath(), 0777); err != nil {
		return err
	}
	cpCommand := fmt.Sprintf("cp %s/*.sql %s", s.testdata("dumper", "input"), s.scratchPath())
	if err := exec.Command("/bin/sh", "-c", cpCommand).Run(); err != nil {
		return err
	}
	dir, err := getDir(s.scratchPath())
	if err != nil {
		return err
	} else if len(dir.LogicalSchemas) != 1 {
		return fmt.Errorf("Unexpected logical schema count for %s: %d", dir, len(dir.LogicalSchemas))
	}
	s.scratchDir = dir
	wsOpts := workspace.Options{
		Type:            workspace.TypeTempSchema,
		Instance:        s.d.Instance,
		CleanupAction:   workspace.CleanupActionDrop,
		SchemaName:      "dumper_test",
		LockWaitTimeout: 30 * time.Second,
	}
	wsSchema, err := workspace.ExecLogicalSchema(dir.LogicalSchemas[0], wsOpts)
	s.schema, s.statementErrors = wsSchema.Schema, wsSchema.Failures
	return err
}

// testdata returns the absolute path of the testdata dir, or a file or dir
// based from it
func (s *IntegrationSuite) testdata(joins ...string) string {
	parts := append([]string{"..", "testdata"}, joins...)
	result := filepath.Join(parts...)
	if cleaned, err := filepath.Abs(filepath.Clean(result)); err == nil {
		return cleaned
	}
	return result
}

// scratchPath returns the scratch directory for tests to write temporary files
// to.
func (s *IntegrationSuite) scratchPath() string {
	return s.testdata(".scratch")
}

// reparseScratchDir updates the logical schema stored in the test suite, to
// reflect any changes made in the filesystem.
func (s *IntegrationSuite) reparseScratchDir(t *testing.T) {
	t.Helper()
	dir, err := getDir(s.scratchPath())
	if err != nil {
		t.Fatalf("Unexpected error parsing scratch dir: %v", err)
	} else if len(dir.LogicalSchemas) != 1 {
		t.Fatalf("Unexpected logical schema count for %s: %d", dir, len(dir.LogicalSchemas))
	}
	s.scratchDir = dir
}

// verifyFormat confirms that the SQL files in the scratch directory match
// those in the golden directory.
func (s *IntegrationSuite) verifyFormat(t *testing.T) {
	t.Helper()

	s.reparseScratchDir(t)
	goldenDir, err := getDir(s.testdata("dumper", "golden"))
	if err != nil {
		t.Fatalf("Unable to obtain golden dir: %v", err)
	}

	// Compare *.sql files
	if len(s.scratchDir.SQLFiles) != len(goldenDir.SQLFiles) {
		t.Errorf("Differing count of *.sql files between %s and %s", s.scratchDir, goldenDir)
	} else {
		for n := range s.scratchDir.SQLFiles {
			if s.scratchDir.SQLFiles[n].FileName != goldenDir.SQLFiles[n].FileName {
				t.Errorf("Differing file name at position[%d]: %s vs %s", n, s.scratchDir.SQLFiles[n].FileName, goldenDir.SQLFiles[n].FileName)
			}
		}
	}

	// Compare parsed CREATEs
	if len(s.scratchDir.LogicalSchemas) != len(goldenDir.LogicalSchemas) {
		t.Errorf("Mismatch between count of parsed logical schemas: %s=%d vs %s=%d", s.scratchDir, len(s.scratchDir.LogicalSchemas), goldenDir, len(goldenDir.LogicalSchemas))
	} else if len(s.scratchDir.LogicalSchemas) > 0 {
		aCreates, bCreates := s.scratchDir.LogicalSchemas[0].Creates, goldenDir.LogicalSchemas[0].Creates
		if len(aCreates) != len(bCreates) {
			t.Errorf("Mismatch in CREATE count: %s=%d, %s=%d", s.scratchDir, len(aCreates), goldenDir, len(bCreates))
		} else {
			for key, aStmt := range aCreates {
				bStmt := bCreates[key]
				if aStmt.Text != bStmt.Text {
					t.Errorf("Mismatch for %s:\n%s:\n%s\n\n%s:\n%s\n", key, aStmt.Location(), aStmt.Text, bStmt.Location(), bStmt.Text)
				}
			}
		}
	}
}

// getDir parses and returns an *fs.Dir
func getDir(dirPath string) (*fs.Dir, error) {
	cmd := mybase.NewCommand("dumpertest", "", "", nil)
	util.AddGlobalOptions(cmd)
	cfg := &mybase.Config{
		CLI: &mybase.CommandLine{Command: cmd},
	}
	return fs.ParseDir(dirPath, cfg)
}
