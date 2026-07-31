package storage

import (
	"path/filepath"
	"testing"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
)

// newPebbleEngine returns an in-memory Pebble instance for contract testing.
func newPebbleEngine(t *testing.T) DB {
	t.Helper()
	db, err := NewPebble(config.Pebble{DataPath: ""}, zap.NewNop())
	if err != nil {
		t.Fatalf("pebble factory: %v", err)
	}
	return db
}

// newBoltEngine returns a temp-file Bolt instance for contract testing.
func newBoltEngine(t *testing.T) DB {
	t.Helper()
	db, err := NewBoltDB(config.Bolt{
		DataPath: filepath.Join(t.TempDir(), "contract.db"),
	})
	if err != nil {
		t.Fatalf("bolt factory: %v", err)
	}
	return db
}

func TestPebbleContract(t *testing.T) {
	suite.Run(t, &DBContractSuite{NewEngine: newPebbleEngine})
}

func TestBoltContract(t *testing.T) {
	suite.Run(t, &DBContractSuite{NewEngine: newBoltEngine})
}
