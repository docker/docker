package container // import "github.com/docker/docker/container"

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/google/uuid"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

var root string

func TestMain(m *testing.M) {
	var err error
	root, err = ioutil.TempDir("", "docker-container-test-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)

	os.Exit(m.Run())
}

func newContainer(t *testing.T) *Container {
	var (
		id    = uuid.New().String()
		cRoot = filepath.Join(root, id)
	)
	if err := os.MkdirAll(cRoot, 0755); err != nil {
		t.Fatal(err)
	}
	c := NewBaseContainer(id, cRoot, filepath.Join(root, "exec", id))
	c.HostConfig = &containertypes.HostConfig{}
	return c
}

func TestViewSaveDelete(t *testing.T) {
	db, err := NewViewDB()
	if err != nil {
		t.Fatal(err)
	}
	c := newContainer(t)
	if err := c.CheckpointTo(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(c); err != nil {
		t.Fatal(err)
	}
}

func TestViewAll(t *testing.T) {
	var (
		db, _ = NewViewDB()
		one   = newContainer(t)
		two   = newContainer(t)
	)
	one.Pid = 10
	if err := one.CheckpointTo(db); err != nil {
		t.Fatal(err)
	}
	two.Pid = 20
	if err := two.CheckpointTo(db); err != nil {
		t.Fatal(err)
	}

	all, err := db.Snapshot().All()
	if err != nil {
		t.Fatal(err)
	}
	if l := len(all); l != 2 {
		t.Fatalf("expected 2 items, got %d", l)
	}
	byID := make(map[string]Snapshot)
	for i := range all {
		byID[all[i].ID] = all[i]
	}
	if s, ok := byID[one.ID]; !ok || s.Pid != 10 {
		t.Fatalf("expected something different with for id=%s: %v", one.ID, s)
	}
	if s, ok := byID[two.ID]; !ok || s.Pid != 20 {
		t.Fatalf("expected something different with for id=%s: %v", two.ID, s)
	}
}

func TestViewGet(t *testing.T) {
	var (
		db, _ = NewViewDB()
		one   = newContainer(t)
	)
	one.ImageID = "some-image-123"
	if err := one.CheckpointTo(db); err != nil {
		t.Fatal(err)
	}
	s, err := db.Snapshot().Get(one.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil || s.ImageID != "some-image-123" {
		t.Fatalf("expected ImageID=some-image-123. Got: %v", s)
	}
}

func TestNames(t *testing.T) {
	db, err := NewViewDB()
	if err != nil {
		t.Fatal(err)
	}
	assert.Check(t, db.ReserveName("name1", "containerid1"))
	assert.Check(t, db.ReserveName("name1", "containerid1")) // idempotent
	assert.Check(t, db.ReserveName("name2", "containerid2"))
	assert.Check(t, is.Error(db.ReserveName("name2", "containerid3"), ErrNameReserved.Error()))

	// Releasing a name allows the name to point to something else later.
	assert.Check(t, db.ReleaseName("name2"))
	assert.Check(t, db.ReserveName("name2", "containerid3"))

	view := db.Snapshot()

	id, err := view.GetID("name1")
	assert.Check(t, err)
	assert.Check(t, is.Equal("containerid1", id))

	id, err = view.GetID("name2")
	assert.Check(t, err)
	assert.Check(t, is.Equal("containerid3", id))

	_, err = view.GetID("notreserved")
	assert.Check(t, is.Error(err, ErrNameNotReserved.Error()))

	// Releasing and re-reserving a name doesn't affect the snapshot.
	assert.Check(t, db.ReleaseName("name2"))
	assert.Check(t, db.ReserveName("name2", "containerid4"))

	id, err = view.GetID("name1")
	assert.Check(t, err)
	assert.Check(t, is.Equal("containerid1", id))

	id, err = view.GetID("name2")
	assert.Check(t, err)
	assert.Check(t, is.Equal("containerid3", id))

	// GetAllNames
	assert.Check(t, is.DeepEqual(map[string][]string{"containerid1": {"name1"}, "containerid3": {"name2"}}, view.GetAllNames()))

	assert.Check(t, db.ReserveName("name3", "containerid1"))
	assert.Check(t, db.ReserveName("name4", "containerid1"))

	view = db.Snapshot()
	assert.Check(t, is.DeepEqual(map[string][]string{"containerid1": {"name1", "name3", "name4"}, "containerid4": {"name2"}}, view.GetAllNames()))

	// Release containerid1's names with Delete even though no container exists
	assert.Check(t, db.Delete(&Container{ID: "containerid1"}))

	// Reusing one of those names should work
	assert.Check(t, db.ReserveName("name1", "containerid4"))
	view = db.Snapshot()
	assert.Check(t, is.DeepEqual(map[string][]string{"containerid4": {"name1", "name2"}}, view.GetAllNames()))
}

// Test case for GitHub issue 35920
func TestViewWithHealthCheck(t *testing.T) {
	var (
		db, _ = NewViewDB()
		one   = newContainer(t)
	)
	one.Health = &Health{
		Health: types.Health{
			Status: "starting",
		},
	}
	if err := one.CheckpointTo(db); err != nil {
		t.Fatal(err)
	}
	s, err := db.Snapshot().Get(one.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil || s.Health != "starting" {
		t.Fatalf("expected Health=starting. Got: %+v", s)
	}
}
