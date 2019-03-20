// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package storage

import (
	"fmt"
	"sync"
	"time"

	"github.com/m3db/m3/src/dbnode/generated/thrift/rpc"
	"github.com/m3db/m3/src/dbnode/storage/index"
	"github.com/m3db/m3/src/m3ninx/idx"
	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3x/ident"
)

// QueryConversionCache represents the query conversion LRU cache.
type QueryConversionCache struct {
	sync.RWMutex

	lru *QueryConversionLRU
}

// NewQueryConversionCache creates a new QueryConversionCache with a provided LRU cache.
func NewQueryConversionCache(lru *QueryConversionLRU) *QueryConversionCache {
	return &QueryConversionCache{
		lru: lru,
	}
}

func (q *QueryConversionCache) set(k []byte, v idx.Query) bool {
	return q.lru.Set(k, v)
}

func (q *QueryConversionCache) get(k []byte) (idx.Query, bool) {
	return q.lru.Get(k)
}

// FromM3IdentToMetric converts an M3 ident metric to a coordinator metric.
func FromM3IdentToMetric(
	identID ident.ID,
	iterTags ident.TagIterator,
	tagOptions models.TagOptions,
) (models.Metric, error) {
	tags, err := FromIdentTagIteratorToTags(iterTags, tagOptions)
	if err != nil {
		return models.Metric{}, err
	}

	return models.Metric{
		ID:   identID.Bytes(),
		Tags: tags,
	}, nil
}

// FromIdentTagIteratorToTags converts ident tags to coordinator tags.
func FromIdentTagIteratorToTags(
	identTags ident.TagIterator,
	tagOptions models.TagOptions,
) (models.Tags, error) {
	tags := models.NewTags(identTags.Remaining(), tagOptions)
	for identTags.Next() {
		identTag := identTags.Current()
		tags = tags.AddTag(models.Tag{
			Name:  identTag.Name.Bytes(),
			Value: identTag.Value.Bytes(),
		})
	}

	if err := identTags.Err(); err != nil {
		return models.EmptyTags(), err
	}

	return tags, nil
}

// TagsToIdentTagIterator converts coordinator tags to ident tags.
func TagsToIdentTagIterator(tags models.Tags) ident.TagIterator {
	// TODO: get a tags and tag iterator from an ident.Pool here rather than allocing them here
	identTags := make([]ident.Tag, 0, tags.Len())
	for _, t := range tags.Tags {
		identTags = append(identTags, ident.Tag{
			Name:  ident.BytesID(t.Name),
			Value: ident.BytesID(t.Value),
		})
	}

	return ident.NewTagsIterator(ident.NewTags(identTags...))
}

// FetchOptionsToM3Options converts a set of coordinator options to M3 options.
func FetchOptionsToM3Options(fetchOptions *FetchOptions, fetchQuery *FetchQuery) index.QueryOptions {
	return index.QueryOptions{
		Limit:          fetchOptions.Limit,
		StartInclusive: fetchQuery.Start,
		EndExclusive:   fetchQuery.End,
	}
}

func convertAggregateQueryType(completeNameOnly bool) rpc.AggregateQueryType {
	if completeNameOnly {
		return rpc.AggregateQueryType_AGGREGATE_BY_TAG_NAME
	}

	return rpc.AggregateQueryType_AGGREGATE_BY_TAG_NAME_VALUE
}

// FetchOptionsToAggregateOptions converts a set of coordinator options as well
// as complete tags query to an M3 aggregate query option.
func FetchOptionsToAggregateOptions(
	fetchOptions *FetchOptions,
	fetchQuery *CompleteTagsQuery,
) index.AggregateQueryOptions {
	return index.AggregateQueryOptions{
		QueryOptions: index.QueryOptions{
			Limit:          fetchOptions.Limit,
			StartInclusive: time.Time{},
			EndExclusive:   time.Now(),
		},
		TagNameFilter:      fetchQuery.FilterNameTags,
		AggregateQueryType: convertAggregateQueryType(fetchQuery.CompleteNameOnly),
	}
}

var (
	// byte representation for [1,2,3,4]
	lookup = [4]byte{49, 50, 51, 52}
)

func queryKey(m models.Matchers) []byte {
	l := len(m)
	for _, t := range m {
		l += len(t.Name) + len(t.Value)
	}

	key := make([]byte, l)
	idx := 0
	for _, t := range m {
		idx += copy(key[idx:], t.Name)
		key[idx] = lookup[t.Type]
		idx += copy(key[idx+1:], t.Value)
		idx++
	}

	return key
}

// FetchQueryToM3Query converts an m3coordinator fetch query to an M3 query.
func FetchQueryToM3Query(
	fetchQuery *FetchQuery,
	cache *QueryConversionCache,
) (index.Query, error) {
	matchers := fetchQuery.TagMatchers
	// If no matchers provided, explicitly set this to an AllQuery
	if len(matchers) == 0 {
		return index.Query{
			// TODO: change this to an idx.AllQuery: https://github.com/m3db/m3/pull/1478
			Query: idx.Query{},
		}, nil
	}

	k := queryKey(matchers)
	cache.RLock()

	if val, ok := cache.get(k); ok {
		cache.RUnlock()
		return index.Query{Query: val}, nil
	}

	cache.RUnlock()
	// Optimization for single matcher case.
	if len(matchers) == 1 {
		q, err := matcherToQuery(matchers[0])
		if err != nil {
			return index.Query{}, err
		}

		cache.Lock()
		cache.set(k, q)
		cache.Unlock()
		return index.Query{Query: q}, nil
	}

	idxQueries := make([]idx.Query, len(matchers))
	var err error
	for i, matcher := range matchers {
		idxQueries[i], err = matcherToQuery(matcher)
		if err != nil {
			return index.Query{}, err
		}
	}

	q := idx.NewConjunctionQuery(idxQueries...)
	cache.Lock()
	cache.set(k, q)
	cache.Unlock()

	return index.Query{Query: q}, nil
}

func matcherToQuery(matcher models.Matcher) (idx.Query, error) {
	negate := false
	switch matcher.Type {
	// Support for Regexp types
	case models.MatchNotRegexp:
		negate = true
		fallthrough
	case models.MatchRegexp:
		query, err := idx.NewRegexpQuery(matcher.Name, matcher.Value)
		if err != nil {
			return idx.Query{}, err
		}
		if negate {
			query = idx.NewNegationQuery(query)
		}
		return query, nil

		// Support exact matches
	case models.MatchNotEqual:
		negate = true
		fallthrough
	case models.MatchEqual:
		query := idx.NewTermQuery(matcher.Name, matcher.Value)
		if negate {
			query = idx.NewNegationQuery(query)
		}
		return query, nil

	default:
		return idx.Query{}, fmt.Errorf("unsupported query type: %v", matcher)
	}
}
