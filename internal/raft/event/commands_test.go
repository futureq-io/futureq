package raft

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type CommandsSuite struct {
	suite.Suite
}

func TestCommandsSuite(t *testing.T) {
	suite.Run(t, new(CommandsSuite))
}

// ─── StoreBatchCmd round-trips ──────────────────────────────────────────────

func (s *CommandsSuite) TestStoreBatchCmd_BasicRoundTrip() {
	require := s.Require()

	items := []StoreBatchItem{
		{Bucket: 100, TopicHash: 42, Msg: []byte("hello")},
		{Bucket: 200, TopicHash: 43, Msg: []byte("world")},
	}

	cmd, err := MarshalStoreBatchCmd(items)
	require.NoError(err)
	require.Equal(byte(StoreBatchCmd), cmd[0])

	got, err := UnmarshalStoreBatchCmd(cmd)
	require.NoError(err)
	require.Len(got, 2)
	require.Equal(uint64(100), got[0].Bucket)
	require.Equal(uint64(42), got[0].TopicHash)
	require.Equal([]byte("hello"), got[0].Msg)
	require.Equal(uint64(200), got[1].Bucket)
	require.Equal(uint64(43), got[1].TopicHash)
	require.Equal([]byte("world"), got[1].Msg)
}

func (s *CommandsSuite) TestStoreBatchCmd_WithIndexes() {
	require := s.Require()

	items := []StoreBatchItem{
		{
			Bucket:    5,
			TopicHash: 99,
			Indexes:   [][]byte{[]byte("idx1"), []byte("idx2")},
			Msg:       []byte("msg"),
		},
	}

	cmd, err := MarshalStoreBatchCmd(items)
	require.NoError(err)

	got, err := UnmarshalStoreBatchCmd(cmd)
	require.NoError(err)
	require.Len(got, 1)
	require.Len(got[0].Indexes, 2)
	require.Equal([]byte("idx1"), got[0].Indexes[0])
	require.Equal([]byte("idx2"), got[0].Indexes[1])
}

func (s *CommandsSuite) TestStoreBatchCmd_EmptyItemList() {
	require := s.Require()

	cmd, err := MarshalStoreBatchCmd([]StoreBatchItem{})
	require.NoError(err)

	got, err := UnmarshalStoreBatchCmd(cmd)
	require.NoError(err)
	require.Empty(got)
}

func (s *CommandsSuite) TestStoreBatchCmd_EmptyMsg() {
	require := s.Require()

	items := []StoreBatchItem{{Bucket: 1, TopicHash: 1, Msg: []byte{}}}
	cmd, err := MarshalStoreBatchCmd(items)
	require.NoError(err)

	got, err := UnmarshalStoreBatchCmd(cmd)
	require.NoError(err)
	require.Len(got, 1)
	require.Empty(got[0].Msg)
}

func (s *CommandsSuite) TestStoreBatchCmd_TooShort_Fails() {
	require := s.Require()

	_, err := UnmarshalStoreBatchCmd([]byte{byte(StoreBatchCmd), 0x01})
	require.Error(err)
}

func (s *CommandsSuite) TestStoreBatchCmd_WrongType_Fails() {
	require := s.Require()

	deleteCmd, _ := MarshalDeleteBatchCmd([][]byte{make([]byte, 24)})

	_, err := UnmarshalStoreBatchCmd(deleteCmd)
	require.Error(err)
}

func (s *CommandsSuite) TestStoreBatchCmd_TruncatedMsg_Fails() {
	require := s.Require()

	items := []StoreBatchItem{{Bucket: 1, TopicHash: 1, Msg: []byte("hello-world")}}
	cmd, _ := MarshalStoreBatchCmd(items)

	// Cut buffer before end of msg.
	_, err := UnmarshalStoreBatchCmd(cmd[:len(cmd)-3])
	require.Error(err)
}

// ─── DeleteBatchCmd round-trips ──────────────────────────────────────────────

func (s *CommandsSuite) TestDeleteBatchCmd_BasicRoundTrip() {
	require := s.Require()

	keys := [][]byte{
		make([]byte, 24),
		make([]byte, 24),
	}
	// Make keys distinct.
	keys[0][0] = 0xAA
	keys[1][0] = 0xBB

	cmd, err := MarshalDeleteBatchCmd(keys)
	require.NoError(err)
	require.Equal(byte(DeleteBatchCmd), cmd[0])

	got, err := UnmarshalDeleteBatchCmd(cmd)
	require.NoError(err)
	require.Len(got, 2)
	require.Equal(keys[0], got[0])
	require.Equal(keys[1], got[1])
}

func (s *CommandsSuite) TestDeleteBatchCmd_InvalidKeyLength_Fails() {
	require := s.Require()

	_, err := MarshalDeleteBatchCmd([][]byte{[]byte("short")})
	require.Error(err)
}

func (s *CommandsSuite) TestDeleteBatchCmd_EmptyList() {
	require := s.Require()

	cmd, err := MarshalDeleteBatchCmd([][]byte{})
	require.NoError(err)

	got, err := UnmarshalDeleteBatchCmd(cmd)
	require.NoError(err)
	require.Empty(got)
}

func (s *CommandsSuite) TestDeleteBatchCmd_TooShort_Fails() {
	require := s.Require()

	_, err := UnmarshalDeleteBatchCmd([]byte{byte(DeleteBatchCmd), 0x00})
	require.Error(err)
}

func (s *CommandsSuite) TestDeleteBatchCmd_WrongType_Fails() {
	require := s.Require()

	storeCmd, _ := MarshalStoreBatchCmd([]StoreBatchItem{{Bucket: 1, TopicHash: 1, Msg: []byte("x")}})

	_, err := UnmarshalDeleteBatchCmd(storeCmd)
	require.Error(err)
}

func (s *CommandsSuite) TestDeleteBatchCmd_TruncatedKeyData_Fails() {
	require := s.Require()

	keys := [][]byte{make([]byte, 24)}
	cmd, _ := MarshalDeleteBatchCmd(keys)

	// Cut mid-key.
	_, err := UnmarshalDeleteBatchCmd(cmd[:len(cmd)-10])
	require.Error(err)
}
