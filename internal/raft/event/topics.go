package raft

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/futureq-io/futureq/internal/storage"
)

const (
	// Keep catalog keys longer than event keys, even for one-character names.
	topicPrefix             = "metadata/registered-topics/"
	MaxInitialSubscriptions = 256
)

var ErrTopicNotFound = errors.New("topic does not exist")

var errTopicCatalogUnavailable = errors.New("topic catalog unavailable while snapshot recovery is incomplete")

// TopicCatalogQuery looks up Name, or lists a page after After when Name is empty.
type TopicCatalogQuery struct {
	Name  string
	After string
	Limit int
}

type TopicCatalogResult struct {
	Exists bool
	Names  []string
	Next   string
}

func (q TopicCatalogQuery) readForLookup(db storage.DB) (any, error) {
	return q.Read(db)
}

// Read queries a consistent catalog view. Clustered callers use Lookup, which
// guards this read against snapshot replacement.
func (q TopicCatalogQuery) Read(db storage.DB) (TopicCatalogResult, error) {
	if q.Name != "" {
		exists, err := TopicExists(db, q.Name)
		return TopicCatalogResult{Exists: exists}, err
	}
	names, next, err := ListTopics(db, q.After, q.Limit)
	return TopicCatalogResult{Names: names, Next: next}, err
}

func ValidateTopicName(name string) error {
	if len(name) == 0 || len(name) > 255 || !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("topic name must be valid UTF-8, contain 1 to 255 bytes, and contain no control characters")
	}
	return nil
}

func TopicExists(db storage.DB, name string) (bool, error) {
	_, closer, err := db.Get([]byte(topicPrefix + name))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, closer.Close()
}

// ListTopics returns a page in ascending name order and an exclusive cursor
// when more topics are available.
func ListTopics(db storage.DB, after string, limit int) ([]string, string, error) {
	lower := []byte(topicPrefix)
	if after != "" {
		lower = []byte(topicPrefix + after + "\x00")
	}
	iter, err := db.NewIter(&storage.IterOptions{
		LowerBound: lower,
		UpperBound: []byte(topicPrefix[:len(topicPrefix)-1] + "0"),
	})
	if err != nil {
		return nil, "", err
	}
	defer iter.Close() //nolint:errcheck

	names := make([]string, 0, limit)
	valid := iter.First()
	for valid && len(names) < limit {
		names = append(names, string(iter.Key()[len(topicPrefix):]))
		valid = iter.Next()
	}
	if err := iter.Error(); err != nil {
		return nil, "", err
	}
	if valid {
		return names, names[len(names)-1], nil
	}
	return names, "", nil
}

func (b *DeliveryBatch) topicExists(name string) (bool, error) {
	if _, exists := b.createdTopics[name]; exists {
		return true, nil
	}
	return TopicExists(b.db, name)
}

func (b *DeliveryBatch) createTopic(name string) error {
	exists, err := b.topicExists(name)
	if err != nil || exists {
		return err
	}
	if err := b.batch.Set([]byte(topicPrefix+name), []byte{1}); err != nil {
		return err
	}
	if b.createdTopics == nil {
		b.createdTopics = make(map[string]struct{})
	}
	b.createdTopics[name] = struct{}{}
	return nil
}
