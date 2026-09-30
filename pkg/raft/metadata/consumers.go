package metadata

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ConsumerMember is the cluster-wide identity of one group subscription.
type ConsumerMember struct {
	ID     string `json:"id"`
	NodeID uint64 `json:"node_id"`
}

// ConsumerState is replicated by the metadata Raft group. Members are sorted
// by node ID, then by registration order within a node.
type ConsumerState struct {
	Groups   map[string]map[string][]ConsumerMember `json:"groups"`
	Epoch    uint64                              `json:"epoch"`
	Pending  bool                                `json:"pending"`
	Required []uint64                            `json:"required"`
	Acked    map[uint64]bool                     `json:"acked"`
}

type consumerChange struct {
	Operation string   `json:"operation"`
	Topic     string   `json:"topic,omitempty"`
	Group     string   `json:"group,omitempty"`
	Member    ConsumerMember `json:"member,omitempty"`
	NodeID    uint64   `json:"node_id,omitempty"`
	LocalIDs  []string `json:"local_ids,omitempty"`
	Required  []uint64 `json:"required"`
}

type consumerAck struct {
	Epoch  uint64 `json:"epoch"`
	NodeID uint64 `json:"node_id"`
}

func marshalConsumerChange(change consumerChange) ([]byte, error) {
	if change.Operation != "add" && change.Operation != "remove" && change.Operation != "reconcile" {
		return nil, fmt.Errorf("metadata: invalid consumer operation %q", change.Operation)
	}
	data, err := json.Marshal(change)
	if err != nil {
		return nil, err
	}
	return append([]byte{byte(ConsumerChangeCmd)}, data...), nil
}

func marshalConsumerAck(epoch, nodeID uint64) ([]byte, error) {
	data, err := json.Marshal(consumerAck{Epoch: epoch, NodeID: nodeID})
	if err != nil {
		return nil, err
	}
	return append([]byte{byte(ConsumerAckCmd)}, data...), nil
}

func (c *ConsumerState) normalize() {
	if c.Groups == nil {
		c.Groups = make(map[string]map[string][]ConsumerMember)
	}
	if c.Acked == nil {
		c.Acked = make(map[uint64]bool)
	}
	for topic := range c.Groups {
		for group := range c.Groups[topic] {
			sort.SliceStable(c.Groups[topic][group], func(i, j int) bool {
				return c.Groups[topic][group][i].NodeID < c.Groups[topic][group][j].NodeID
			})
		}
	}
}

func (c *ConsumerState) startRebalance(required []uint64) {
	c.Epoch++
	c.Pending = true
	c.Acked = make(map[uint64]bool)
	c.Required = append([]uint64(nil), required...)
	sort.Slice(c.Required, func(i, j int) bool { return c.Required[i] < c.Required[j] })
	c.Required = uniqueNodeIDs(c.Required)
	if len(c.Required) == 0 {
		c.Pending = false
	}
}

func uniqueNodeIDs(ids []uint64) []uint64 {
	n := 0
	for _, id := range ids {
		if n == 0 || ids[n-1] != id {
			ids[n] = id
			n++
		}
	}
	return ids[:n]
}

func (c *ConsumerState) applyChange(change consumerChange) bool {
	c.normalize()
	changed := false
	switch change.Operation {
	case "add":
		if change.Topic == "" || change.Group == "" || change.Member.ID == "" {
			return false
		}
		if c.Groups[change.Topic] == nil {
			c.Groups[change.Topic] = make(map[string][]ConsumerMember)
		}
		for _, member := range c.Groups[change.Topic][change.Group] {
			if member.ID == change.Member.ID {
				return false
			}
		}
		c.Groups[change.Topic][change.Group] = append(c.Groups[change.Topic][change.Group], change.Member)
		changed = true
	case "remove":
		members := c.Groups[change.Topic][change.Group]
		for i, member := range members {
			if member.ID == change.Member.ID {
				c.Groups[change.Topic][change.Group] = append(members[:i], members[i+1:]...)
				changed = true
				break
			}
		}
		if len(c.Groups[change.Topic][change.Group]) == 0 {
			delete(c.Groups[change.Topic], change.Group)
		}
		if len(c.Groups[change.Topic]) == 0 {
			delete(c.Groups, change.Topic)
		}
	case "reconcile":
		local := make(map[string]bool, len(change.LocalIDs))
		for _, id := range change.LocalIDs {
			local[id] = true
		}
		for topic, groups := range c.Groups {
			for group, members := range groups {
				kept := members[:0]
				for _, member := range members {
					if member.NodeID == change.NodeID && !local[member.ID] {
						changed = true
						continue
					}
					kept = append(kept, member)
				}
				if len(kept) == 0 {
					delete(groups, group)
				} else {
					groups[group] = kept
				}
			}
			if len(groups) == 0 {
				delete(c.Groups, topic)
			}
		}
	default:
		return false
	}
	if changed {
		c.normalize()
		c.startRebalance(change.Required)
	}
	return changed
}

func (c *ConsumerState) acknowledge(ack consumerAck) {
	if !c.Pending || ack.Epoch != c.Epoch {
		return
	}
	needed := false
	for _, id := range c.Required {
		if id == ack.NodeID {
			needed = true
			break
		}
	}
	if !needed {
		return
	}
	c.Acked[ack.NodeID] = true
	for _, id := range c.Required {
		if !c.Acked[id] {
			return
		}
	}
	c.Pending = false
}
