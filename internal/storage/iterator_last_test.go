package storage

func (s *DBContractSuite) TestIteratorLastRespectsBoundsAndRestarts() {
	require := s.Require()
	batch := s.db.NewBatch()
	for _, key := range []string{"a", "c", "e"} {
		require.NoError(batch.Set([]byte(key), []byte(key)))
	}
	require.NoError(batch.Commit(Sync))
	require.NoError(batch.Close())
	for _, tc := range []struct{ lower, upper, first, last string }{
		{"", "", "a", "e"}, {"b", "e", "c", "c"}, {"b", "d", "c", "c"},
		{"c", "z", "c", "e"}, {"b", "c", "", ""}, {"f", "", "", ""}, {"", "a", "", ""},
	} {
		opts := &IterOptions{}
		if tc.lower != "" {
			opts.LowerBound = []byte(tc.lower)
		}
		if tc.upper != "" {
			opts.UpperBound = []byte(tc.upper)
		}
		iter, err := s.db.NewIter(opts)
		require.NoError(err)
		require.Equal(tc.last != "", iter.Last(), "bounds %+v", tc)
		if tc.last != "" {
			require.Equal(tc.last, string(iter.Key()))
			require.False(iter.Next())
		}
		require.Equal(tc.first != "", iter.First(), "First must restart after Last/Next exhausts the iterator")
		if tc.first != "" {
			require.Equal(tc.first, string(iter.Key()))
		}
		require.Equal(tc.last != "", iter.Last())
		if tc.last != "" {
			require.Equal(tc.last, string(iter.Key()))
		}
		require.NoError(iter.Error())
		require.NoError(iter.Close())
	}
}
