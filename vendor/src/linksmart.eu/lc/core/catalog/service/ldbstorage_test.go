package service

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/pborman/uuid"
)

func setupLevelDB() (CatalogStorage, string, error) {
	tempDir := fmt.Sprintf("%s/lslc/test-%s.ldb",
		strings.Replace(os.TempDir(), "\\", "/", -1), uuid.New())
	storage, err := NewLevelDBStorage(tempDir, nil)
	if err != nil {
		return nil, tempDir, err
	}
	return storage, tempDir, nil
}

func TestLevelDBAddService(t *testing.T) {
	storage, tempDir, err := setupLevelDB()
	if err != nil {
		t.Fatal(err.Error())
	}
	defer os.RemoveAll(tempDir)
	defer storage.Close()

	r := &Service{}
	uuid := "E9203BE9-D705-42A8-8B12-F28E7EA2FC99"
	r.Name = "ServiceName"
	r.Id = uuid + "/" + r.Name
	r.Ttl = 30

	err = storage.add(*r)
	if err != nil {
		t.Errorf("Received unexpected error: %v", err.Error())
	}
}

func TestLevelDBUpdateService(t *testing.T) {
	storage, tempDir, err := setupLevelDB()
	if err != nil {
		t.Fatal(err.Error())
	}
	defer os.RemoveAll(tempDir)
	defer storage.Close()

	r := &Service{}
	uuid := "E9203BE9-D705-42A8-8B12-F28E7EA2FC99"
	r.Name = "ServiceName"
	r.Id = uuid + "/" + r.Name
	r.Ttl = 30

	err = storage.add(*r)
	if err != nil {
		t.Errorf("Unexpected error on add: %v", err.Error())
	}
	r.Name = "UpdatedName"

	err = storage.update(r.Id, *r)
	if err != nil {
		t.Errorf("Unexpected error on update: %v", err.Error())
	}

	rg, err := storage.get(r.Id)
	if err != nil {
		t.Error("Unexpected error on get: %v", err.Error())
	}

	if rg.Name != r.Name {
		t.Fail()
	}
}

func TestLevelDBGetService(t *testing.T) {
	storage, tempDir, err := setupLevelDB()
	if err != nil {
		t.Fatal(err.Error())
	}
	defer os.RemoveAll(tempDir)
	defer storage.Close()

	r := &Service{
		Name: "TestName",
	}
	uuid := "E9203BE9-D705-42A8-8B12-F28E7EA2FC99"
	r.Name = "ServiceName"
	r.Id = uuid + "/" + r.Name
	r.Ttl = 30

	err = storage.add(*r)
	if err != nil {
		t.Errorf("Unexpected error on add: %v", err.Error())
	}

	rg, err := storage.get(r.Id)
	if err != nil {
		t.Error("Unexpected error on get: %v", err.Error())
	}

	if rg.Id != r.Id || rg.Name != r.Name || rg.Ttl != r.Ttl {
		t.Fail()
	}
}

func TestLevelDBDeleteService(t *testing.T) {
	storage, tempDir, err := setupLevelDB()
	if err != nil {
		t.Fatal(err.Error())
	}
	defer os.RemoveAll(tempDir)
	defer storage.Close()

	r := &Service{}
	uuid := "E9203BE9-D705-42A8-8B12-F28E7EA2FC99"
	r.Name = "ServiceName"
	r.Id = uuid + "/" + r.Name
	r.Ttl = 30
	err = storage.add(*r)
	if err != nil {
		t.Errorf("Unexpected error on add: %v", err.Error())
	}

	err = storage.delete(r.Id)
	if err != nil {
		t.Error("Unexpected error on delete: %v", err.Error())
	}

	err = storage.delete(r.Id)
	if err != ErrorNotFound {
		t.Error("The previous call hasn't deleted the Service?")
	}
}

func TestLevelDBGetManyServices(t *testing.T) {
	storage, tempDir, err := setupLevelDB()
	if err != nil {
		t.Fatal(err.Error())
	}
	defer os.RemoveAll(tempDir)
	defer storage.Close()

	r := &Service{}
	// Add 10 entries
	for i := 0; i < 11; i++ {
		r.Name = string(i)
		r.Id = "TestID" + "/" + r.Name
		r.Ttl = 30
		err := storage.add(*r)

		if err != nil {
			t.Errorf("Unexpected error on add: %v", err.Error())
		}
	}

	p1pp2, total, _ := storage.getMany(1, 2)
	if total != 11 {
		t.Errorf("Expected total is 11, returned: %v", total)
	}

	if len(p1pp2) != 2 {
		t.Errorf("Wrong number of entries: requested page=1 , perPage=2. Expected: 2, returned: %v", len(p1pp2))
	}

	p2pp2, _, _ := storage.getMany(2, 2)
	if len(p2pp2) != 2 {
		t.Errorf("Wrong number of entries: requested page=2 , perPage=2. Expected: 2, returned: %v", len(p2pp2))
	}

	p2pp5, _, _ := storage.getMany(2, 5)
	if len(p2pp5) != 5 {
		t.Errorf("Wrong number of entries: requested page=2 , perPage=5. Expected: 5, returned: %v", len(p2pp5))
	}

	p4pp3, _, _ := storage.getMany(4, 3)
	if len(p4pp3) != 2 {
		t.Errorf("Wrong number of entries: requested page=4 , perPage=3. Expected: 2, returned: %v", len(p4pp3))
	}
}
